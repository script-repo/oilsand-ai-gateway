#!/usr/bin/env bash
# Unattended Hermes + Telegram gateway setup for a Rocky/Ubuntu worker.
#
# Mirrors the script the TUI generates (tui/agents.go) so the same flow can be
# run by hand outside the TUI. It:
#   1. bootstraps deps (git, Node.js, npm prefix on PATH),
#   2. installs the Hermes CLI via the official installer if missing,
#   3. points Hermes at an Olla OpenAI endpoint (custom provider),
#   4. writes the Telegram credentials to ~/.hermes/.env (mode 600),
#   5. installs + starts the messaging gateway non-interactively, and
#   6. prints `hermes gateway status` for verification.
#
# Run as the unprivileged service user (e.g. rocky); GATEWAY_MODE=system uses
# sudo for a boot-level root service.
#
# Required env:
#   HERMES_OLLA_BASE   Olla OpenAI base, e.g. http://gateway-host:40114/olla/openai/v1
#   HERMES_OLLA_MODEL  default model, e.g. minimax-m3:cloud
#   TELEGRAM_BOT_TOKEN BotFather token
# Optional env:
#   HERMES_OLLA_KEY        Olla API token (default: olla)
#   TELEGRAM_ALLOWED_USERS comma-separated Telegram user IDs (blank = pair by DM)
#   TELEGRAM_HOME_CHANNEL  default chat ID for notifications
#   GATEWAY_MODE           user (default) | system
set -euo pipefail
export HERMES_NONINTERACTIVE=1

: "${HERMES_OLLA_BASE:?set HERMES_OLLA_BASE (Olla OpenAI base URL)}"
: "${HERMES_OLLA_MODEL:?set HERMES_OLLA_MODEL (default model)}"
: "${TELEGRAM_BOT_TOKEN:?set TELEGRAM_BOT_TOKEN (BotFather token)}"
HERMES_OLLA_KEY="${HERMES_OLLA_KEY:-olla}"
TELEGRAM_ALLOWED_USERS="${TELEGRAM_ALLOWED_USERS:-}"
TELEGRAM_HOME_CHANNEL="${TELEGRAM_HOME_CHANNEL:-}"
GATEWAY_MODE="${GATEWAY_MODE:-user}"

log() { printf '[setup-hermes] %s\n' "$*"; }

# --- 1. deps: git, node, npm global prefix on PATH --------------------------
log "ensuring deps (git, Node.js, npm prefix)…"
. /etc/os-release 2>/dev/null || true
if ! command -v git >/dev/null 2>&1; then
  case "${ID:-}" in
    ubuntu|debian)
      sudo apt-get update -y >/dev/null 2>&1 || true
      sudo apt-get install -y git
      ;;
    *)
      sudo dnf install -y git >/dev/null 2>&1 || sudo yum install -y git
      ;;
  esac
fi
if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
  case "${ID:-}" in
    ubuntu|debian)
      curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
      sudo apt-get install -y nodejs
      ;;
    *)
      curl -fsSL https://rpm.nodesource.com/setup_22.x | sudo -E bash -
      sudo dnf install -y nodejs >/dev/null 2>&1 || sudo yum install -y nodejs
      ;;
  esac
fi
if command -v npm >/dev/null 2>&1; then
  npm config set prefix "$HOME/.npm-global" >/dev/null 2>&1 || true
  mkdir -p "$HOME/.npm-global/bin" "$HOME/.local/bin"
  case ":$PATH:" in
    *":$HOME/.npm-global/bin:"*) : ;;
    *) export PATH="$HOME/.npm-global/bin:$PATH"
       grep -q '.npm-global/bin' "$HOME/.bashrc" 2>/dev/null || \
         echo 'export PATH="$HOME/.npm-global/bin:$PATH"' >> "$HOME/.bashrc" ;;
  esac
fi
export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:$PATH"
grep -q '.local/bin' "$HOME/.bashrc" 2>/dev/null || \
  echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$HOME/.bashrc"

# --- 2. install hermes if missing ------------------------------------------
if ! command -v hermes >/dev/null 2>&1; then
  log "installing hermes (official installer, skip setup)…"
  curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash -s -- --skip-setup
  export PATH="/usr/local/bin:$HOME/.local/bin:$HOME/.npm-global/bin:$PATH"
  hash -r 2>/dev/null || true
fi
if ! command -v hermes >/dev/null 2>&1 && command -v ollama >/dev/null 2>&1; then
  log "hermes still missing; trying ollama launch hermes…"
  ollama launch hermes --yes --model "$HERMES_OLLA_MODEL" </dev/null || true
  export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:$PATH"
fi
if ! command -v hermes >/dev/null 2>&1; then
  log "ERROR: hermes not found on PATH after install"
  exit 1
fi
HERMES_BIN="$(command -v hermes)"
log "hermes: $HERMES_BIN"

# --- 3. point hermes at Olla -----------------------------------------------
log "pointing hermes at Olla ($HERMES_OLLA_BASE)…"
PY="$HOME/.hermes/hermes-agent/venv/bin/python"; [ -x "$PY" ] || PY=python3
OLLA_BASE="$HERMES_OLLA_BASE" OLLA_KEY="$HERMES_OLLA_KEY" OLLA_MODEL="$HERMES_OLLA_MODEL" "$PY" - <<'PYEOF'
import os, pathlib, yaml
p = pathlib.Path(os.path.expanduser("~/.hermes/config.yaml"))
cfg = yaml.safe_load(p.read_text()) if p.exists() else {}
cfg = cfg or {}
m = cfg.get("model")
if not isinstance(m, dict):
    m = {}
m["provider"] = "custom"
m["base_url"] = os.environ["OLLA_BASE"]
m["api_key"] = os.environ["OLLA_KEY"]
m["default"] = os.environ["OLLA_MODEL"]
m.pop("api_mode", None)
cfg["model"] = m
p.parent.mkdir(parents=True, exist_ok=True)
p.write_text(yaml.safe_dump(cfg, sort_keys=False))
print("[setup-hermes] hermes -> Olla", m["base_url"], m["default"])
PYEOF

# --- 4. write ~/.hermes/.env (Telegram creds, mode 600) --------------------
log "writing Telegram credentials to ~/.hermes/.env…"
TG_TOKEN="$TELEGRAM_BOT_TOKEN" TG_ALLOWED="$TELEGRAM_ALLOWED_USERS" TG_HOME="$TELEGRAM_HOME_CHANNEL" "$PY" - <<'PYEOF'
import os, pathlib, re
p = pathlib.Path(os.path.expanduser("~/.hermes/.env"))
p.parent.mkdir(parents=True, exist_ok=True)
text = p.read_text() if p.exists() else ""
updates = {
    "TELEGRAM_BOT_TOKEN": os.environ.get("TG_TOKEN", ""),
    "TELEGRAM_ALLOWED_USERS": os.environ.get("TG_ALLOWED", ""),
    "TELEGRAM_HOME_CHANNEL": os.environ.get("TG_HOME", ""),
}
keys = [k for k, v in updates.items() if v]
out = []
for ln in text.splitlines():
    s = ln.lstrip()
    if any(re.match(r"^#?\s*" + re.escape(k) + r"\s*=", s) for k in keys):
        continue
    out.append(ln)
for k in ("TELEGRAM_BOT_TOKEN", "TELEGRAM_ALLOWED_USERS", "TELEGRAM_HOME_CHANNEL"):
    if updates[k]:
        out.append(k + "=" + updates[k])
p.write_text("\n".join(out).rstrip("\n") + "\n")
os.chmod(p, 0o600)
print("[setup-hermes] .env updated:", ",".join(keys) or "(none)")
PYEOF

# --- 5. install + start the gateway (non-interactive) ----------------------
if [ "$GATEWAY_MODE" = "system" ]; then
  log "installing gateway (system service)…"
  sudo HERMES_NONINTERACTIVE=1 HOME="$HOME" "$HERMES_BIN" gateway install --system --run-as-user "$USER" </dev/null
else
  log "installing gateway (user service)…"
  HERMES_NONINTERACTIVE=1 "$HERMES_BIN" gateway install </dev/null
  loginctl enable-linger "$USER" >/dev/null 2>&1 || true
  HERMES_NONINTERACTIVE=1 "$HERMES_BIN" gateway start </dev/null || true
fi

# --- 6. verify --------------------------------------------------------------
log "gateway status:"
HERMES_NONINTERACTIVE=1 "$HERMES_BIN" gateway status </dev/null 2>&1 | head -20 || true
log "done — Hermes gateway is configured for Telegram."
