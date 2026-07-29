package main

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// Nanoclaw is deployed differently from the other worker agents: instead of a
// host install it runs as one or more Docker containers on the chosen worker,
// so several isolated instances can share the same VM. The image is built once
// per worker from the upstream NanoClaw repo; each instance gets an
// auto-incremented name (nanoclaw-01, nanoclaw-02, …) and its own state volume,
// and is pointed at the Olla Anthropic endpoint (via OneCLI) so agents
// load-balance across the whole pool.

const nanoclawImage = "oilsand/nanoclaw:latest"

// nanoclawDockerfileLabel records, on the built image, the sha256 of the
// Dockerfile it was built from. Deploys compare it against the staged
// Dockerfile so a worker bootstrapped by an older TUI rebuilds instead of
// stamping out containers from a stale image (e.g. one without the upgrade
// marker, which NanoClaw's tripwire refuses to start).
const nanoclawDockerfileLabel = "oilsand.dockerfile-sha256"

// nanoclawDockerfile builds the per-worker Nanoclaw image from the upstream
// project. Upstream is TypeScript managed with pnpm ("start" runs
// dist/index.js), so the image must compile it — pnpm install + pnpm run build
// — before the runtime CMD. The store directory is a volume so each container
// instance keeps its own persistent state.
//
// NanoClaw itself sandboxes every agent in a container of its own, so the
// image carries a full inner Docker engine (docker-in-docker) that the
// entrypoint starts before NanoClaw. Mounting the host's docker socket
// instead would not work: NanoClaw bind-mounts paths from its own filesystem
// (groups/, data/, …) into agent containers, and a host daemon would resolve
// those paths against the host, where they don't exist.
const nanoclawDockerfile = `# Nanoclaw agent image, built by the Oilsand AI Gateway TUI.
# Every deployed instance is a separate container from this one image.
# NOTE: do not multi-stage COPY from ghcr.io/block/buzz — that image ships
# buzz-relay/buzz-admin only (no buzz-cli). A missing COPY path fails the build.
FROM node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl openssl \
 && rm -rf /var/lib/apt/lists/*
# Inner Docker engine: NanoClaw refuses to start without a container runtime
# ("docker info" must succeed inside this container) and uses it to sandbox
# its agents. Requires the instance to run with --privileged.
RUN curl -fsSL https://get.docker.com | sh \
 && rm -rf /var/lib/apt/lists/*
# Upstream pins pnpm via the packageManager field; corepack provides it.
RUN corepack enable || npm install -g pnpm
RUN git clone --depth 1 https://github.com/qwibitai/nanoclaw /opt/nanoclaw
WORKDIR /opt/nanoclaw
# Oilsand Olla provider: inject OpenAI-compatible Olla base URL + placeholder
# key into agent containers. OneCLI rewrites Authorization on the wire.
# Trunk's empty providers/index.ts never loads custom endpoints without this.
COPY <<'OLLA_PROVIDER_EOF' src/providers/oilsand-olla.ts
import { readEnvFile } from '../env.js';
import { registerProviderContainerConfig } from './provider-container-registry.js';

registerProviderContainerConfig('claude', () => {
  const dotenv = readEnvFile(['ANTHROPIC_BASE_URL', 'OPENAI_BASE_URL', 'OPENAI_API_KEY']);
  const env: Record<string, string> = {};
  // Claude Agent SDK speaks Anthropic Messages API → Olla /olla/anthropic.
  // OneCLI rewrites Authorization using the vault secret for this host.
  if (dotenv.ANTHROPIC_BASE_URL) {
    env.ANTHROPIC_BASE_URL = dotenv.ANTHROPIC_BASE_URL;
    env.ANTHROPIC_AUTH_TOKEN = 'placeholder';
  }
  // Also pass OpenAI-compatible base for tools/SDKs that speak chat/completions.
  if (dotenv.OPENAI_BASE_URL) {
    env.OPENAI_BASE_URL = dotenv.OPENAI_BASE_URL;
    env.OPENAI_API_KEY = dotenv.OPENAI_API_KEY || 'placeholder';
    env.OPENAI_API_BASE = dotenv.OPENAI_BASE_URL;
  }
  return { env };
});
OLLA_PROVIDER_EOF
RUN grep -q "oilsand-olla" src/providers/index.ts \
 || printf "\nimport './oilsand-olla.js';\n" >> src/providers/index.ts
RUN pnpm install --frozen-lockfile || pnpm install
RUN pnpm run build
# Put the ncl admin CLI on PATH. Upstream's setup normally symlinks bin/ncl
# into ~/.local/bin; we do it here so an interactive shell in this container
# can just run "ncl groups list" without knowing the project layout.
RUN chmod +x bin/ncl 2>/dev/null || true
RUN ln -sf /opt/nanoclaw/bin/ncl /usr/local/bin/ncl
# A fresh clone has no upgrade marker, and NanoClaw's upgrade tripwire refuses
# to start without one (it assumes an unsanctioned git pull). Recording the
# just-built version marks this image as a sanctioned install. Guarded so
# older upstream revisions without the tripwire still build.
RUN [ ! -f scripts/upgrade-state.ts ] || pnpm exec tsx scripts/upgrade-state.ts set
# Wire OneCLI + .env so NanoClaw agents call Olla OpenAI. Idempotent; state on volume.
COPY <<'OLLA_CFG_EOF' /usr/local/bin/oilsand-configure-olla.sh
#!/bin/bash
set -euo pipefail
log() { printf '[oilsand-olla] %s\n' "$*"; }

cd /opt/nanoclaw
mkdir -p /opt/nanoclaw/store

# Persist .env on the named state volume so ONECLI_API_KEY and friends survive
# outer-container recreate (only /opt/nanoclaw/store and inner docker volumes
# are durable; the image filesystem is wiped on every recreate).
ENV_FILE="/opt/nanoclaw/store/oilsand-nanoclaw.env"
touch "$ENV_FILE"
ln -sfn "$ENV_FILE" /opt/nanoclaw/.env

# Claude Agent SDK → Anthropic Messages API at Olla. Also keep OpenAI base for
# tools that speak chat/completions. Prefer explicit env; derive the pair.
ANTH_URL="${OILSAND_OLLA_ANTHROPIC_URL:-${ANTHROPIC_BASE_URL:-}}"
OPEN_URL="${OILSAND_OLLA_OPENAI_URL:-${OPENAI_BASE_URL:-}}"
TOKEN="${OILSAND_OLLA_TOKEN:-${ANTHROPIC_AUTH_TOKEN:-${OPENAI_API_KEY:-olla}}}"
MODEL="${OILSAND_OLLA_MODEL:-${NANOCLAW_MODEL:-}}"
GW_ROOT=""

# Derive both shapes from whichever URL the deploy passed.
for cand in "$ANTH_URL" "$OPEN_URL"; do
  [ -n "$cand" ] || continue
  base="${cand%/}"
  base="${base%/v1}"
  base="$(printf '%s' "$base" | sed -E 's|/olla/(anthropic|openai)(/v1)?$||')"
  GW_ROOT="$base"
  break
done

if [ -z "$GW_ROOT" ]; then
  log "no Olla URL set (OILSAND_OLLA_ANTHROPIC_URL / OILSAND_OLLA_OPENAI_URL); skipping"
  exit 0
fi

ANTH_URL="${GW_ROOT}/olla/anthropic"
OPEN_URL="${GW_ROOT}/olla/openai/v1"
HOST="$(printf '%s' "$GW_ROOT" | sed -E 's|^[a-zA-Z][a-zA-Z0-9+.-]*://([^/:]+).*|\1|')"
if [ -z "$HOST" ] || [ "$HOST" = "$GW_ROOT" ]; then
  log "ERROR: could not parse hostname from $GW_ROOT"
  exit 1
fi
log "target Olla Anthropic API: $ANTH_URL (Claude Agent SDK)"
log "target Olla OpenAI API:    $OPEN_URL (OpenAI-compatible tools)"
log "gateway host for OneCLI:   $HOST"

upsert_env() {
  local key="$1" val="$2" file="$ENV_FILE"
  touch "$file"
  if grep -q "^${key}=" "$file" 2>/dev/null; then
    grep -v "^${key}=" "$file" > "${file}.tmp" || true
    mv "${file}.tmp" "$file"
  fi
  printf '%s=%s\n' "$key" "$val" >> "$file"
}

# Claude brain → Anthropic path; tools may use OpenAI path. OneCLI vault holds the token.
upsert_env ANTHROPIC_BASE_URL "$ANTH_URL"
upsert_env OPENAI_BASE_URL "$OPEN_URL"
upsert_env OPENAI_API_KEY "placeholder"
if [ -n "$MODEL" ]; then
  upsert_env OILSAND_OLLA_MODEL "$MODEL"
fi

# --- OneCLI install (gateway compose + CLI; both required) -----------------
export PATH="/usr/local/bin:/root/.local/bin:$HOME/.local/bin:$PATH"
if [ ! -f "$HOME/.onecli/docker-compose.yml" ] && [ ! -f /root/.onecli/docker-compose.yml ]; then
  log "installing OneCLI gateway…"
  if ! curl -fsSL https://onecli.sh/install | sh; then
    log "ERROR: OneCLI gateway install failed"
    exit 1
  fi
fi
if ! command -v onecli >/dev/null 2>&1; then
  log "installing OneCLI CLI…"
  if ! curl -fsSL https://onecli.sh/cli/install | sh; then
    log "ERROR: OneCLI CLI install failed"
    exit 1
  fi
  export PATH="/usr/local/bin:/root/.local/bin:$HOME/.local/bin:$PATH"
fi
if ! command -v onecli >/dev/null 2>&1; then
  log "ERROR: onecli not on PATH after install"
  exit 1
fi

# Resolve the dashboard URL. Prefer an already-configured api-host, else the
# local default the installer binds.
ONECLI_URL="$(onecli config get api-host 2>/dev/null | tr -d '\r' || true)"
if printf '%s' "$ONECLI_URL" | grep -q 'https\?://'; then
  :
else
  ONECLI_URL="$(printf '%s' "$ONECLI_URL" | grep -oE 'https?://[^[:space:]"]+' | head -n1 || true)"
fi
if [ -z "$ONECLI_URL" ]; then
  ONECLI_URL="http://127.0.0.1:10254"
  onecli config set api-host "$ONECLI_URL" >/dev/null 2>&1 || true
fi
log "OneCLI URL: $ONECLI_URL"
# Publish for the outer TUI / docker logs (operator-facing GUI URL is set at deploy).
printf 'OILSAND_ONECLI_INTERNAL_URL %s\n' "$ONECLI_URL" >> /var/log/oilsand-olla.log

# Bring the compose stack up if the installer left it stopped (common after
# outer-container recreate when only the inner docker volumes persist).
COMPOSE=""
for cand in "$HOME/.onecli/docker-compose.yml" /root/.onecli/docker-compose.yml; do
  if [ -f "$cand" ]; then COMPOSE="$cand"; break; fi
done
if [ -n "$COMPOSE" ]; then
  docker compose -f "$COMPOSE" up -d >/dev/null 2>&1 || true
fi

# Wait for the vault to answer (compose bring-up can take a minute).
ok=0
for _ in $(seq 1 90); do
  if curl -fsS "$ONECLI_URL/health" >/dev/null 2>&1 \
     || curl -fsS "$ONECLI_URL/api/health" >/dev/null 2>&1 \
     || curl -fsS "$ONECLI_URL/v1/health" >/dev/null 2>&1; then
    ok=1
    break
  fi
  sleep 2
done
if [ "$ok" -ne 1 ]; then
  log "ERROR: OneCLI did not become healthy at $ONECLI_URL"
  if [ -n "$COMPOSE" ]; then
    docker compose -f "$COMPOSE" ps >&2 || true
  fi
  exit 1
fi
log "OneCLI is healthy"

upsert_env ONECLI_URL "$ONECLI_URL"

# NanoClaw's container-runner refuses to spawn agents unless applyContainerConfig
# succeeds, which needs ONECLI_API_KEY in .env. Prefer a previously persisted
# key; otherwise ask the local CLI / bootstrap from gateway logs.
API_KEY=""
if grep -q '^ONECLI_API_KEY=' "$ENV_FILE" 2>/dev/null; then
  API_KEY="$(grep '^ONECLI_API_KEY=' "$ENV_FILE" | head -n1 | cut -d= -f2-)"
fi
if [ -z "$API_KEY" ]; then
  API_KEY="$(onecli auth api-key 2>/dev/null | tr -d '\r' | grep -oE 'oc_[A-Za-z0-9_]+' | head -n1 || true)"
fi
if [ -z "$API_KEY" ]; then
  # Fresh local installs often print the bootstrap key once in container logs.
  for c in $(docker ps --format '{{.Names}}' 2>/dev/null | grep -i onecli || true); do
    API_KEY="$(docker logs "$c" 2>&1 | grep -oE 'oc_[A-Za-z0-9_]+' | head -n1 || true)"
    [ -n "$API_KEY" ] && break
  done
fi
if [ -n "$API_KEY" ]; then
  upsert_env ONECLI_API_KEY "$API_KEY"
  onecli auth login --api-key "$API_KEY" >/dev/null 2>&1 || true
  log "ONECLI_API_KEY present in .env"
else
  log "WARNING: could not discover ONECLI_API_KEY — agent spawns may refuse until one is set"
fi

# Register the Olla host as a generic Bearer secret (same shape as NanoClaw
# setup's custom-endpoint auth). Idempotent: delete+recreate on conflict.
log "registering OneCLI secret for host $HOST…"
onecli secrets delete --name OilsandOlla >/dev/null 2>&1 || true
if ! onecli secrets create \
    --name OilsandOlla \
    --type generic \
    --value "$TOKEN" \
    --host-pattern "$HOST" \
    --header-name Authorization \
    --value-format 'Bearer {value}'; then
  log "ERROR: onecli secrets create failed"
  exit 1
fi
log "OneCLI secret OilsandOlla → host-pattern $HOST"

# Best-effort: grant the secret to every existing agent so a recreated
# Nanoclaw that already has groups can call Olla immediately. Also loop in
# the background for agents NanoClaw creates after ensureAgent on first spawn.
grant_olla_secret() {
  SECRET_ID="$(onecli secrets list 2>/dev/null | grep -i OilsandOlla | head -n1 | awk '{print $1}' || true)"
  [ -n "$SECRET_ID" ] || return 0
  onecli agents list 2>/dev/null | while read -r line; do
    aid="$(printf '%s' "$line" | awk '{print $1}')"
    [ -n "$aid" ] || continue
    printf '%s' "$aid" | grep -qE '^[a-zA-Z0-9-]+$' || continue
    onecli agents set-secrets --id "$aid" --secret-ids "$SECRET_ID" >/dev/null 2>&1 || true
  done
}
grant_olla_secret || true
(
  for _ in $(seq 1 30); do
    sleep 10
    grant_olla_secret || true
  done
) >> /var/log/oilsand-olla.log 2>&1 &

# Optional: once ncl.sock is up, pin the default group model to the pool model.
if [ -n "$MODEL" ]; then
  (
    for _ in $(seq 1 60); do
      [ -S /opt/nanoclaw/data/ncl.sock ] && break
      sleep 2
    done
    if [ -S /opt/nanoclaw/data/ncl.sock ]; then
      gid="$(ncl groups list --json 2>/dev/null | grep -oE '"id"[[:space:]]*:[[:space:]]*"[^"]+"' | head -n1 | sed -E 's/.*"([^"]+)"/\1/' || true)"
      if [ -n "$gid" ]; then
        ncl groups config update --id "$gid" --model "$MODEL" >/dev/null 2>&1 \
          && log "set group $gid model=$MODEL" \
          || log "could not set group model (non-fatal)"
      fi
    fi
  ) >> /var/log/oilsand-olla.log 2>&1 &
fi

log "Olla wiring complete — .env has ANTHROPIC_BASE_URL + OPENAI_BASE_URL + ONECLI_URL"
OLLA_CFG_EOF
RUN chmod +x /usr/local/bin/oilsand-configure-olla.sh
# Join the Oilsand Buzz relay (block/buzz) as this instance. Presence only —
# keeps a Nostr key on the state volume, sets online, joins the channel.
# Backgrounded and non-fatal so an unreachable relay never stops NanoClaw.
COPY <<'BUZZ_JOIN_EOF' /usr/local/bin/oilsand-join-buzz.sh
#!/bin/bash
set -euo pipefail
log() { printf '[oilsand-buzz] %s\n' "$*"; }

RELAY="${OILSAND_BUZZ_RELAY_URL:-}"
CHAN_ID="${OILSAND_BUZZ_CHANNEL_ID:-}"
CHAN_NAME="${OILSAND_BUZZ_CHANNEL:-oilsand}"
NAME="${OILSAND_BUZZ_NAME:-nanoclaw}"

if [ -z "$RELAY" ]; then
  log "no OILSAND_BUZZ_RELAY_URL; skipping"
  exit 0
fi
if ! command -v buzz >/dev/null 2>&1; then
  log "buzz-cli not on PATH; skipping (rebuild image from multi-stage buzz copy)"
  exit 0
fi

mkdir -p /opt/nanoclaw/store
KEY_FILE="/opt/nanoclaw/store/oilsand-buzz.key"
if [ ! -s "$KEY_FILE" ]; then
  openssl rand -hex 32 > "$KEY_FILE"
  chmod 600 "$KEY_FILE"
  log "generated per-instance Buzz key at $KEY_FILE"
fi
export BUZZ_RELAY_URL="$RELAY"
export BUZZ_PRIVATE_KEY="$(tr -d '\r\n' < "$KEY_FILE")"
export PATH="/usr/local/bin:$PATH"

log "relay=$RELAY name=$NAME channel=${CHAN_ID:-$CHAN_NAME}"

# Wait for the relay (gateway may still be starting).
ok=0
for _ in $(seq 1 30); do
  if curl -fsS "${RELAY%/}/_liveness" >/dev/null 2>&1 \
     || curl -fsS "${RELAY%/}/health" >/dev/null 2>&1; then
    ok=1
    break
  fi
  sleep 2
done
if [ "$ok" -ne 1 ]; then
  log "WARNING: relay not reachable at $RELAY — will keep retrying presence"
fi

# Resolve channel id by name when deploy did not pin one.
if [ -z "$CHAN_ID" ]; then
  LIST="$(buzz channels list 2>/dev/null || true)"
  CHAN_ID="$(printf '%s' "$LIST" | grep -i "\"name\"[[:space:]]*:[[:space:]]*\"$CHAN_NAME\"" -B5 -A5 \
    | grep -oE '[0-9a-f-]{36}' | head -n1 || true)"
  if [ -z "$CHAN_ID" ]; then
    OUT="$(buzz channels create --name "$CHAN_NAME" --type stream --visibility open 2>/dev/null || true)"
    CHAN_ID="$(printf '%s' "$OUT" | grep -oE '[0-9a-f-]{36}' | head -n1 || true)"
  fi
fi
if [ -n "$CHAN_ID" ]; then
  buzz channels join --channel "$CHAN_ID" >/dev/null 2>&1 || true
  printf '%s\n' "$CHAN_ID" > /opt/nanoclaw/store/oilsand-buzz-channel.id
  log "joined channel $CHAN_ID"
else
  log "WARNING: could not resolve Buzz channel $CHAN_NAME"
fi

# Keep presence alive; exit quietly on permanent CLI failure after backoff.
backoff=5
while true; do
  buzz users set-presence --status online >/dev/null 2>&1 \
    && log "presence online as $NAME" \
    || log "set-presence failed (retry in ${backoff}s)"
  sleep 60
  backoff=5
done
BUZZ_JOIN_EOF
RUN chmod +x /usr/local/bin/oilsand-join-buzz.sh
# Entrypoint: bring up the inner dockerd, wire Olla via OneCLI, join Buzz,
# then hand off to CMD. The cgroup shuffle mirrors the official docker:dind
# entrypoint — on cgroup v2 dockerd can only enable controllers for child
# cgroups once the root cgroup's processes have moved into a leaf.
COPY <<'ENTRYPOINT_EOF' /usr/local/bin/nanoclaw-entrypoint.sh
#!/bin/sh
set -e
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  mkdir -p /sys/fs/cgroup/init
  xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init/cgroup.procs || true
  sed -e 's/ / +/g' -e 's/^/+/' < /sys/fs/cgroup/cgroup.controllers \
    > /sys/fs/cgroup/cgroup.subtree_control || true
fi
dockerd > /var/log/dockerd.log 2>&1 &
i=0
until docker info >/dev/null 2>&1; do
  i=$((i+1))
  if [ "$i" -ge 60 ]; then
    echo "[entrypoint] inner dockerd failed to start (is the container privileged?):" >&2
    tail -n 50 /var/log/dockerd.log >&2 || true
    exit 1
  fi
  sleep 1
done
# Persist host DB + agent group folders on the named state volume so CLI
# wirings and memory survive outer-container recreate (image FS is wiped).
mkdir -p /opt/nanoclaw/store
for d in data groups; do
  if [ -L "/opt/nanoclaw/$d" ]; then
    :
  else
    if [ -d "/opt/nanoclaw/$d" ] && [ ! -e "/opt/nanoclaw/store/$d" ]; then
      mv "/opt/nanoclaw/$d" "/opt/nanoclaw/store/$d"
    fi
    mkdir -p "/opt/nanoclaw/store/$d"
    rm -rf "/opt/nanoclaw/$d"
    ln -sfn "/opt/nanoclaw/store/$d" "/opt/nanoclaw/$d"
  fi
done
# OneCLI + .env → Olla. Failure is loud but non-fatal for the outer process:
# NanoClaw still starts (Buzz/shell work), and agent spawns will surface the
# OneCLI error clearly in logs if wiring did not complete.
if [ -n "${OILSAND_OLLA_ANTHROPIC_URL:-${OILSAND_OLLA_OPENAI_URL:-${ANTHROPIC_BASE_URL:-${OPENAI_BASE_URL:-}}}}" ]; then
  echo "[entrypoint] configuring OneCLI → Olla…"
  /usr/local/bin/oilsand-configure-olla.sh >> /var/log/oilsand-olla.log 2>&1 \
    || echo "[entrypoint] WARNING: oilsand-configure-olla.sh failed — see /var/log/oilsand-olla.log" >&2
fi
# Surface OneCLI GUI URL for docker logs / operators (host port published at deploy).
if [ -n "${OILSAND_ONECLI_PUBLISH_URL:-}" ]; then
  echo "[entrypoint] OneCLI dashboard (from host): ${OILSAND_ONECLI_PUBLISH_URL}"
  echo "OILSAND_ONECLI_URL ${OILSAND_ONECLI_PUBLISH_URL}" >> /var/log/oilsand-olla.log
fi
# Join Buzz (block/buzz) when the TUI deployed coordinates. Backgrounded and
# non-fatal: an unreachable relay must never stop NanoClaw from starting.
if [ -n "${OILSAND_BUZZ_RELAY_URL:-}" ]; then
  echo "[entrypoint] joining Buzz at ${OILSAND_BUZZ_RELAY_URL} as ${OILSAND_BUZZ_NAME:-nanoclaw}"
  /usr/local/bin/oilsand-join-buzz.sh >> /var/log/oilsand-buzz.log 2>&1 &
fi
# Start the host, wait for CLI socket, ensure a cli/local agent exists, then
# stay as PID 1 waiting on the host. Init is idempotent and non-fatal.
(
  i=0
  while [ ! -S /opt/nanoclaw/data/cli.sock ]; do
    i=$((i+1))
    [ "$i" -ge 90 ] && exit 0
    sleep 1
  done
  cd /opt/nanoclaw
  export PATH="/opt/nanoclaw/bin:/usr/local/bin:$PATH"
  if [ ! -f /opt/nanoclaw/store/oilsand-cli-agent.ok ]; then
    echo "[entrypoint] initializing CLI channel agent…"
    pnpm exec tsx scripts/init-cli-agent.ts \
      --display-name "${OILSAND_CLI_USER:-operator}" \
      --agent-name "${OILSAND_CLI_AGENT:-Nano}" \
      >> /var/log/oilsand-cli-agent.log 2>&1 \
      && touch /opt/nanoclaw/store/oilsand-cli-agent.ok \
      || echo "[entrypoint] WARNING: init-cli-agent failed — see /var/log/oilsand-cli-agent.log" >&2
  fi
) &
exec "$@"
ENTRYPOINT_EOF
RUN chmod +x /usr/local/bin/nanoclaw-entrypoint.sh
# Interactive CLI-channel chat loop used by the TUI "open" action.
COPY <<'CHAT_EOF' /usr/local/bin/oilsand-nanoclaw-chat.sh
#!/bin/bash
# Talk to the NanoClaw agent via the built-in CLI channel (data/cli.sock).
# Upstream pnpm run chat is one-shot; this wraps it in a read loop.
set -e
cd /opt/nanoclaw
export PATH="/opt/nanoclaw/bin:/usr/local/bin:$HOME/.local/bin:$PATH"
export PS1="nanoclaw-chat:\w\$ "

admin_shell() {
  export PATH="/opt/nanoclaw/bin:$PATH"
  export PS1="nanoclaw:\w\$ "
  echo "Admin shell. ncl is one-shot admin (not chat):"
  echo "  ncl help | ncl groups list | ncl sessions list"
  echo "  exit returns to Oilsand TUI"
  exec bash --norc -i
}

echo "Waiting for NanoClaw CLI channel (data/cli.sock)…"
ok=0
for _ in $(seq 1 90); do
  if [ -S data/cli.sock ]; then ok=1; break; fi
  sleep 1
done
if [ "$ok" -ne 1 ]; then
  echo "CLI socket never appeared — host may be down. Falling back to admin shell."
  echo "Check: docker logs <container>  and  /var/log/oilsand-olla.log"
  admin_shell
fi

# Ensure an agent is wired to cli/local (idempotent).
if [ ! -f store/oilsand-cli-agent.ok ] || [ ! -f scripts/init-cli-agent.ts ]; then
  if [ -f scripts/init-cli-agent.ts ]; then
    echo "Initializing CLI agent (first connect)…"
    if pnpm exec tsx scripts/init-cli-agent.ts \
         --display-name "${OILSAND_CLI_USER:-operator}" \
         --agent-name "${OILSAND_CLI_AGENT:-Nano}" \
         >> /var/log/oilsand-cli-agent.log 2>&1; then
      mkdir -p store
      touch store/oilsand-cli-agent.ok
    else
      echo "WARNING: init-cli-agent failed — see /var/log/oilsand-cli-agent.log"
      echo "You can still try chatting; if nothing answers, use /shell and ncl."
    fi
  fi
fi

echo ""
echo "NanoClaw CLI chat  (session persists server-side between lines)"
echo "  type a message and press enter"
echo "  /shell   admin shell (ncl)"
echo "  /help    this text"
echo "  /exit    leave (back to Oilsand TUI)"
echo "First reply after a cold start can take 30-60s while the sandbox boots."
echo ""

send_chat() {
  local msg="$1"
  if command -v pnpm >/dev/null 2>&1; then
    pnpm exec tsx scripts/chat.ts "$msg" || true
  else
    npx --yes tsx scripts/chat.ts "$msg" || true
  fi
}

while true; do
  printf 'you> '
  IFS= read -r line || break
  # trim leading/trailing whitespace without bashisms that need backticks
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line%"${line##*[![:space:]]}"}"
  [ -z "$line" ] && continue
  case "$line" in
    /exit|exit|quit|/q)
      break
      ;;
    /shell)
      admin_shell
      ;;
    /help|help)
      echo "Commands: /shell  /help  /exit"
      echo "Anything else is sent to the agent on cli/local."
      ;;
    *)
      send_chat "$line"
      echo ""
      ;;
  esac
done
CHAT_EOF
RUN chmod +x /usr/local/bin/oilsand-nanoclaw-chat.sh
VOLUME /opt/nanoclaw/store
# Inner image/container storage. Must be a volume: the inner dockerd cannot
# run overlay2 on top of the outer container's overlayfs.
VOLUME /var/lib/docker
ENTRYPOINT ["/usr/local/bin/nanoclaw-entrypoint.sh"]
CMD ["node", "dist/index.js"]
`

// dockerBootstrap makes Docker available on the worker (Rocky/RHEL via the
// docker-ce repo, Debian/Ubuntu via get.docker.com) and starts the daemon.
// Idempotent: it is a no-op when docker is already installed and running.
const dockerBootstrap = `echo "[deploy] ensuring Docker…"
if ! command -v docker >/dev/null 2>&1; then
  . /etc/os-release 2>/dev/null || true
  case "${ID:-}" in
    ubuntu|debian)
      curl -fsSL https://get.docker.com | sudo sh
      ;;
    *)
      sudo dnf -y install dnf-plugins-core >/dev/null 2>&1 || true
      sudo dnf config-manager --add-repo https://download.docker.com/linux/rhel/docker-ce.repo 2>/dev/null \
        || sudo dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
      sudo dnf -y install docker-ce docker-ce-cli containerd.io docker-buildx-plugin
      ;;
  esac
fi
sudo systemctl enable --now docker
echo "[deploy] docker: $(sudo docker --version)"
`

// nanoclawDeployScript builds the install/run bootstrap for `instances`
// Nanoclaw containers on one worker: Docker bootstrap, a one-time image build,
// then a run loop that picks the next free nanoclaw-NN name so repeated deploys
// keep adding instances instead of colliding. Each container receives Olla
// Anthropic+OpenAI coordinates (OneCLI) and a published OneCLI dashboard port.
func (m *model) nanoclawDeployScript(instances int) string {
	if instances < 1 {
		instances = 1
	}
	// Claude Agent SDK → /olla/anthropic; OpenAI tools → /olla/openai/v1.
	gw := strings.TrimRight(m.gateway, "/")
	anthropicBase := gw + "/olla/anthropic"
	openaiBase := gw + "/olla/openai/v1"
	key := orDefault(m.token, "olla")
	model := m.effDefaultModel()
	buzzRelay := m.buzzRelayHTTP()
	buzzChan := m.buzzChannelID
	dfB64 := base64.StdEncoding.EncodeToString([]byte(nanoclawDockerfile))

	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(dockerBootstrap)
	b.WriteString(fmt.Sprintf(`NANO_IMG='%s'
mkdir -p "$HOME/.nanoclaw"
echo %s | base64 -d > "$HOME/.nanoclaw/Dockerfile"
DF_SHA="$(sha256sum "$HOME/.nanoclaw/Dockerfile" | cut -d' ' -f1)"
IMG_SHA="$(sudo docker image inspect "$NANO_IMG" --format '{{index .Config.Labels "%s"}}' 2>/dev/null || true)"
if [ "$IMG_SHA" != "$DF_SHA" ]; then
  echo "[deploy] building $NANO_IMG (image missing or built from an outdated Dockerfile)…"
  sudo docker build --label "%s=$DF_SHA" -t "$NANO_IMG" "$HOME/.nanoclaw"
`+nanoclawRecreateFragment+`fi
WORKER_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
`, nanoclawImage, dfB64, nanoclawDockerfileLabel, nanoclawDockerfileLabel))
	b.WriteString(fmt.Sprintf(`started=""
for _i in $(seq 1 %d); do
  n=1
  while sudo docker ps -a --format '{{.Names}}' | grep -qx "nanoclaw-$(printf '%%02d' "$n")"; do n=$((n+1)); done
  NAME="nanoclaw-$(printf '%%02d' "$n")"
  # OneCLI dashboard inside the container listens on 10254; publish a unique host port.
  ONECLI_HOST_PORT=$((10253 + n))
  ONECLI_PUB="http://${WORKER_IP:-127.0.0.1}:$ONECLI_HOST_PORT"
  echo "[deploy] starting container $NAME (OneCLI GUI $ONECLI_PUB)…"
  sudo docker run -d --restart unless-stopped --privileged --name "$NAME" \
    -v "oilsand-$NAME:/opt/nanoclaw/store" \
    -v "oilsand-$NAME-docker:/var/lib/docker" \
    -p "${ONECLI_HOST_PORT}:10254" \
    -e OILSAND_OLLA_ANTHROPIC_URL='%s' \
    -e OILSAND_OLLA_OPENAI_URL='%s' \
    -e OILSAND_OLLA_TOKEN='%s' \
    -e OILSAND_OLLA_MODEL='%s' \
    -e ANTHROPIC_BASE_URL='%s' \
    -e ANTHROPIC_AUTH_TOKEN='%s' \
    -e OPENAI_BASE_URL='%s' \
    -e OPENAI_API_KEY='%s' \
    -e NANOCLAW_MODEL='%s' \
    -e OILSAND_ONECLI_PUBLISH_URL="$ONECLI_PUB" \
    -e OILSAND_BUZZ_RELAY_URL='%s' \
    -e OILSAND_BUZZ_CHANNEL_ID='%s' \
    -e OILSAND_BUZZ_CHANNEL='%s' \
    -e OILSAND_BUZZ_NAME="$NAME" \
    "$NANO_IMG" >/dev/null
  started="$started $NAME"
  echo "OILSAND_ONECLI_URL $NAME $ONECLI_PUB"
done
echo "[deploy] nanoclaw instances on this worker:"
sudo docker ps --filter name=nanoclaw- --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
echo "[deploy] started:$started — each instance is isolated in its own container (state volume oilsand-<name>)."
echo "[deploy] Claude → Olla Anthropic %s ; tools → Olla OpenAI %s (see /var/log/oilsand-olla.log)."
echo "[deploy] OneCLI dashboard is published per instance (see OILSAND_ONECLI_URL lines above)."
echo "[deploy] Buzz join uses %s (see /var/log/oilsand-buzz.log); deploy Buzz from the TUI (ctrl+d) if empty."
`, instances,
		shSingle(anthropicBase), shSingle(openaiBase), shSingle(key), shSingle(model),
		shSingle(anthropicBase), shSingle(key),
		shSingle(openaiBase), shSingle(key), shSingle(model),
		shSingle(buzzRelay), shSingle(buzzChan), buzzChannelName,
		anthropicBase, openaiBase, orDefault(buzzRelay, "(no gateway)")))
	return b.String()
}

// nanoclawShellRC is the bash rc for the /shell fallback inside a Nanoclaw
// container: ncl on PATH plus a short crib sheet.
const nanoclawShellRC = `[ -f /etc/bash.bashrc ] && . /etc/bash.bashrc
export PATH="/opt/nanoclaw/bin:$PATH"
export PS1="nanoclaw:\w\$ "
cd /opt/nanoclaw 2>/dev/null || true
echo "NanoClaw admin shell. ncl is one-shot admin (not chat):"
echo "  ncl help              every resource and verb"
echo "  ncl groups list       agent groups"
echo "  ncl sessions list     active sessions"
echo "  ncl tasks list        scheduled tasks"
echo "Chat is oilsand-nanoclaw-chat.sh (TUI open). Logs:"
echo "  /var/log/oilsand-olla.log   (OneCLI → Olla; look for OneCLI URL)"
echo "  /var/log/oilsand-buzz.log   /var/log/oilsand-cli-agent.log"
if [ -n "${OILSAND_ONECLI_PUBLISH_URL:-}" ]; then
  echo "OneCLI dashboard (host): $OILSAND_ONECLI_PUBLISH_URL"
fi
echo "exit or ctrl-d returns to the TUI."
`

// nanoclawChatFallback is staged on connect when the image is older than the
// baked-in oilsand-nanoclaw-chat.sh. Same loop as the image script: wait for
// cli.sock, init CLI agent, then wrap one-shot pnpm chat in a read loop.
const nanoclawChatFallback = `#!/bin/bash
set -e
cd /opt/nanoclaw
export PATH="/opt/nanoclaw/bin:/usr/local/bin:$PATH"
admin_shell() {
  echo "$OILSAND_SHELL_RC_B64" | base64 -d > /tmp/.oilsand-rc
  exec bash --rcfile /tmp/.oilsand-rc -i
}
echo "Waiting for NanoClaw CLI channel (data/cli.sock)…"
ok=0
for _ in $(seq 1 90); do
  [ -S data/cli.sock ] && ok=1 && break
  sleep 1
done
if [ "$ok" -ne 1 ]; then
  echo "CLI socket never appeared — falling back to admin shell."
  admin_shell
fi
if [ -f scripts/init-cli-agent.ts ] && [ ! -f store/oilsand-cli-agent.ok ]; then
  echo "Initializing CLI agent (first connect)…"
  pnpm exec tsx scripts/init-cli-agent.ts \
    --display-name "${OILSAND_CLI_USER:-operator}" \
    --agent-name "${OILSAND_CLI_AGENT:-Nano}" \
    >> /var/log/oilsand-cli-agent.log 2>&1 \
    && mkdir -p store && touch store/oilsand-cli-agent.ok \
    || echo "WARNING: init-cli-agent failed — see /var/log/oilsand-cli-agent.log"
fi
echo ""
echo "NanoClaw CLI chat  (session persists server-side between lines)"
echo "  type a message and press enter"
echo "  /shell   admin shell (ncl)   /help   /exit"
echo "First reply after a cold start can take 30-60s."
echo ""
while true; do
  printf 'you> '
  IFS= read -r line || break
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line%"${line##*[![:space:]]}"}"
  [ -z "$line" ] && continue
  case "$line" in
    /exit|exit|quit|/q) break ;;
    /shell) admin_shell ;;
    /help|help) echo "Commands: /shell  /help  /exit" ;;
    *) pnpm exec tsx scripts/chat.ts "$line" || true; echo "" ;;
  esac
done
`

// nanoclawConnectRemoteCmd opens the NanoClaw CLI-channel chat inside a
// container. Upstream pnpm run chat is one-shot; we run oilsand-nanoclaw-chat.sh
// (or a staged fallback) which loops it. ncl is never the session command —
// it is a one-shot admin client and would exit immediately.
func nanoclawConnectRemoteCmd(name string) string {
	shellB64 := base64.StdEncoding.EncodeToString([]byte(nanoclawShellRC))
	chatB64 := base64.StdEncoding.EncodeToString([]byte(nanoclawChatFallback))
	// Prefer the image-baked launcher; otherwise stage the fallback loop.
	// OILSAND_SHELL_RC_B64 lets /shell still get the admin crib sheet.
	inner := "export OILSAND_SHELL_RC_B64='" + shellB64 + "'; " +
		"if [ -x /usr/local/bin/oilsand-nanoclaw-chat.sh ]; then " +
		"exec /usr/local/bin/oilsand-nanoclaw-chat.sh; " +
		"fi; " +
		"echo " + chatB64 + " | base64 -d > /tmp/oilsand-nanoclaw-chat.sh; " +
		"chmod +x /tmp/oilsand-nanoclaw-chat.sh; " +
		"exec /tmp/oilsand-nanoclaw-chat.sh"
	return "sudo docker exec -it -w /opt/nanoclaw -e OILSAND_CLI_USER=operator -e OILSAND_CLI_AGENT=Nano '" +
		shSingle(name) + "' bash -c \"" + inner + "\""
}

// nanoclawRecreateFragment recreates every existing nanoclaw container on the
// just-built "$NANO_IMG" image. A plain `docker restart` would leave every
// instance on the image it was created from, so each container is removed and
// re-run — its config env (OPENAI_/ANTHROPIC_/NANOCLAW_/OILSAND_) is carried
// over via docker inspect and its state volumes survive by name. Shared by
// deploy (after an image rebuild) and update. Expects $NANO_IMG to be set and
// contains no fmt verbs, so it can be concatenated into Sprintf formats.
const nanoclawRecreateFragment = `  for c in $(sudo docker ps -a --format '{{.Names}}' | grep '^nanoclaw-' || true); do
    echo "[nanoclaw] recreating $c on the new image"
    ENVF="$(mktemp)"
    sudo docker inspect "$c" --format '{{range .Config.Env}}{{println .}}{{end}}' \
      | grep -E '^(OPENAI_|ANTHROPIC_|NANOCLAW_|OILSAND_)' > "$ENVF" || true
    # Preserve published OneCLI host port if present (hostPort->10254/tcp).
    ONECLI_HP=$(sudo docker inspect "$c" --format '{{range $p,$c := .HostConfig.PortBindings}}{{if eq $p "10254/tcp"}}{{(index $c 0).HostPort}}{{end}}{{end}}' 2>/dev/null || true)
    [ -n "$ONECLI_HP" ] || ONECLI_HP=$(printf '%%s' "$c" | sed -n 's/.*nanoclaw-0*\([0-9][0-9]*\).*/\1/p' | awk '{print 10253+$1}')
    WORKER_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
    if [ -n "$ONECLI_HP" ] && [ -n "$WORKER_IP" ]; then
      if grep -q '^OILSAND_ONECLI_PUBLISH_URL=' "$ENVF" 2>/dev/null; then
        grep -v '^OILSAND_ONECLI_PUBLISH_URL=' "$ENVF" > "${ENVF}.t" || true
        mv "${ENVF}.t" "$ENVF"
      fi
      echo "OILSAND_ONECLI_PUBLISH_URL=http://$WORKER_IP:$ONECLI_HP" >> "$ENVF"
    fi
    sudo docker rm -f "$c" >/dev/null || true
    PORT_ARGS=""
    [ -n "$ONECLI_HP" ] && PORT_ARGS="-p ${ONECLI_HP}:10254"
    sudo docker run -d --restart unless-stopped --privileged --name "$c" \
      -v "oilsand-$c:/opt/nanoclaw/store" \
      -v "oilsand-$c-docker:/var/lib/docker" \
      $PORT_ARGS \
      --env-file "$ENVF" "$NANO_IMG" >/dev/null
    rm -f "$ENVF"
    [ -n "$ONECLI_HP" ] && echo "OILSAND_ONECLI_URL $c http://${WORKER_IP:-127.0.0.1}:$ONECLI_HP"
  done
`

// nanoclawUpdateScript restages the current Dockerfile, rebuilds the image
// against the latest upstream, and recreates the containers on it. The build
// is labeled with the Dockerfile's sha256 (like deploy) so a later deploy
// recognizes the image as current instead of rebuilding it again.
func nanoclawUpdateScript() string {
	dfB64 := base64.StdEncoding.EncodeToString([]byte(nanoclawDockerfile))
	return fmt.Sprintf(`echo '[update] rebuilding nanoclaw image'
NANO_IMG='%s'
mkdir -p "$HOME/.nanoclaw"
echo %s | base64 -d > "$HOME/.nanoclaw/Dockerfile"
DF_SHA="$(sha256sum "$HOME/.nanoclaw/Dockerfile" | cut -d' ' -f1)"
if sudo docker build --pull --no-cache --label "%s=$DF_SHA" -t "$NANO_IMG" "$HOME/.nanoclaw"; then
`+nanoclawRecreateFragment+`else
  echo '[update] rebuild failed — keeping current image and containers'
fi
sudo docker ps --filter name=nanoclaw- --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'`, nanoclawImage, dfB64, nanoclawDockerfileLabel)
}

// nanoclawConnectCmd opens an interactive ncl session inside a named container
// on a worker over SSH. It returns a consoleReadyMsg carrying a `docker exec
// -it` command; the console runner allocates a PTY (ssh -t), so the client's
// interactive UI renders. No registration on exit (the instance already exists).
func nanoclawConnectCmd(user, host, pass, name string) tea.Cmd {
	return func() tea.Msg {
		u := orDefault(user, "rocky")
		if host == "" {
			return notifyMsg("no target host for Nanoclaw")
		}
		if pass != "" {
			_, _ = EnsureKeyAuth(host, u, pass)
		}
		if err := nanoclawPreflight(host, u, pass, name); err != nil {
			return notifyMsg(err.Error())
		}
		return consoleReadyMsg{user: u, host: host, key: managedKeyPath(), cmd: nanoclawConnectRemoteCmd(name), label: "Nanoclaw " + name}
	}
}

// localNanoclawConnectCmd is nanoclawConnectCmd for a container on this machine.
func localNanoclawConnectCmd(name string) tea.Cmd {
	return func() tea.Msg {
		if err := nanoclawPreflight("", "", "", name); err != nil {
			return notifyMsg(err.Error())
		}
		return consoleReadyMsg{local: true, cmd: nanoclawConnectRemoteCmd(name), label: "Nanoclaw " + name}
	}
}

// ---- instance overview (Agents tab, key i) -----------------------------------

// nanoclawPSCmd lists every nanoclaw container (running or not) tab-separated
// for parsing.
const nanoclawPSCmd = `sudo docker ps -a --filter name=nanoclaw- --format '{{.Names}}\t{{.Status}}\t{{.Image}}'`

// nanoclawInstance is one container on one worker.
type nanoclawInstance struct {
	host, name, status, image string
}

type nanoclawInstancesMsg struct {
	rows    []nanoclawInstance
	errs    []string
	hosts   int
	okHosts []string // hosts that answered (even with zero instances)
}

// nanoclawInstancesCmd queries every known Nanoclaw host in parallel and
// returns the merged instance inventory. Hosts that fail (unreachable, no
// docker) are reported as errors without hiding the rest.
func nanoclawInstancesCmd(hosts []string, user, pass string) tea.Cmd {
	return func() tea.Msg {
		msg := nanoclawInstancesMsg{hosts: len(hosts)}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, h := range hosts {
			wg.Add(1)
			go func(h string) {
				defer wg.Done()
				out, err := runNanoclawPS(h, user, pass)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					msg.errs = append(msg.errs, h+": "+err.Error())
					return
				}
				msg.okHosts = append(msg.okHosts, h)
				msg.rows = append(msg.rows, parseNanoclawPS(h, out)...)
			}(h)
		}
		wg.Wait()
		sort.Slice(msg.rows, func(i, j int) bool {
			if msg.rows[i].host != msg.rows[j].host {
				return msg.rows[i].host < msg.rows[j].host
			}
			return msg.rows[i].name < msg.rows[j].name
		})
		sort.Strings(msg.errs)
		sort.Strings(msg.okHosts)
		return msg
	}
}

// runNanoclawHostCmd runs a shell command on one host: locally when the host is
// this machine, over SSH otherwise.
func runNanoclawHostCmd(host, user, pass, script string) (string, error) {
	if isLocalHost(host) {
		out, err := exec.Command("bash", "-lc", script).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%v: %s", err, lastNonEmptyLine(string(out)))
		}
		return string(out), nil
	}
	if pass == "" {
		return "", fmt.Errorf("no SSH password configured")
	}
	client, err := dialSSH(host, user, pass)
	if err != nil {
		return "", err
	}
	defer client.Close()
	out, err := runSSH(client, script)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, lastNonEmptyLine(out))
	}
	return out, nil
}

// runNanoclawPS lists nanoclaw containers on one host.
func runNanoclawPS(host, user, pass string) (string, error) {
	return runNanoclawHostCmd(host, user, pass, nanoclawPSCmd)
}

// nanoclawStatusScript reports a container's state ("running", "exited", …) or
// nothing at all when no such container exists. sudo runs with -n so a host
// that wants a sudo password fails fast instead of blocking the TUI on a
// prompt no one can answer.
func nanoclawStatusScript(name string) string {
	return "sudo -n docker inspect -f '{{.State.Status}}' '" + shSingle(name) + "' 2>/dev/null || true"
}

// nanoclawLogsScript returns the tail of a container's log, for explaining why
// it isn't running.
func nanoclawLogsScript(name string) string {
	return "sudo -n docker logs --tail 5 '" + shSingle(name) + "' 2>&1 || true"
}

// nanoclawStatusError turns an observed container status into the error the
// operator should see, or nil when the container is attachable. Kept separate
// from the host round-trip so the wording is testable on its own.
func nanoclawStatusError(name, where, status, logTail string) error {
	if where == "" {
		where = "this host"
	}
	switch status {
	case "running":
		return nil
	case "":
		return fmt.Errorf("no container %s on %s — deploy Nanoclaw again to recreate it", name, where)
	default:
		if l := strings.TrimSpace(lastNonEmptyLine(logTail)); l != "" {
			return fmt.Errorf("%s is %s on %s — last log: %s", name, status, where, truncate(l, 160))
		}
		return fmt.Errorf("%s is %s on %s (no logs)", name, status, where)
	}
}

// nanoclawPreflight returns nil when the container is up and can be attached
// to, or an explanatory error otherwise.
//
// Without this the console is handed a dead container: docker exec fails in
// the terminal we just gave away, the screen is restored, and all the operator
// sees is "session ended" with the real reason already scrolled off. Checking
// first lets the failure surface as a notice that names the cause.
func nanoclawPreflight(host, user, pass, name string) error {
	where := host
	if where == "" {
		where = "this host"
	}
	out, err := runNanoclawHostCmd(host, user, pass, nanoclawStatusScript(name))
	if err != nil {
		return fmt.Errorf("could not check %s on %s: %v", name, where, err)
	}
	status := strings.TrimSpace(lastNonEmptyLine(out))
	if status == "running" {
		return nil
	}
	var logTail string
	if status != "" {
		// The container's own last log line is nearly always the real reason.
		logTail, _ = runNanoclawHostCmd(host, user, pass, nanoclawLogsScript(name))
	}
	return nanoclawStatusError(name, where, status, logTail)
}

// nanoclawRemoveScript deletes the named containers on one host, optionally
// with their per-instance state volumes. Volume removal is best-effort (a
// volume may never have been created); container removal errors surface.
func nanoclawRemoveScript(names []string, volumes bool) string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "sudo docker rm -f '%s'\n", shSingle(n))
		if volumes {
			fmt.Fprintf(&b, "sudo docker volume rm 'oilsand-%s' >/dev/null 2>&1 || true\n", shSingle(n))
			fmt.Fprintf(&b, "sudo docker volume rm 'oilsand-%s-docker' >/dev/null 2>&1 || true\n", shSingle(n))
		}
	}
	return b.String()
}

// nanoclawRemoveCmd deletes the chosen instances (grouped per host, hosts in
// parallel) and reports the outcome; the caller refreshes the inventory, which
// also reconciles the deployment registration against what is actually left.
func nanoclawRemoveCmd(targets []nanoclawInstance, volumes bool, user, pass string) tea.Cmd {
	byHost := map[string][]string{}
	for _, t := range targets {
		byHost[t.host] = append(byHost[t.host], t.name)
	}
	return func() tea.Msg {
		msg := agentRemovedMsg{agent: "Nanoclaw", container: true}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for h, names := range byHost {
			wg.Add(1)
			go func(h string, names []string) {
				defer wg.Done()
				script := nanoclawRemoveScript(names, volumes)
				var out string
				var err error
				switch {
				case isLocalHost(h):
					b, e := exec.Command("bash", "-lc", script).CombinedOutput()
					out, err = string(b), e
				case pass == "":
					err = fmt.Errorf("no SSH password configured")
				default:
					client, e := dialSSH(h, user, pass)
					if e != nil {
						err = e
					} else {
						out, err = runSSH(client, script)
						client.Close()
					}
				}
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					msg.errs = append(msg.errs, h+": "+strings.TrimSpace(err.Error()+" "+lastNonEmptyLine(out)))
					return
				}
				msg.removed += len(names)
				msg.okHosts = append(msg.okHosts, h)
			}(h, names)
		}
		wg.Wait()
		sort.Strings(msg.okHosts)
		sort.Strings(msg.errs)
		return msg
	}
}

// parseNanoclawPS turns `docker ps` tab-separated output into instances,
// ignoring noise lines (sudo banners, warnings).
func parseNanoclawPS(host, out string) []nanoclawInstance {
	var rows []nanoclawInstance
	for _, ln := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimRight(ln, "\r"), "\t")
		if len(parts) < 3 || !strings.HasPrefix(parts[0], "nanoclaw-") {
			continue
		}
		rows = append(rows, nanoclawInstance{host: host, name: parts[0], status: parts[1], image: parts[2]})
	}
	return rows
}
