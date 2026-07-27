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
// and is pointed at the Olla OpenAI endpoint so it load-balances across the
// whole pool.

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
FROM node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl \
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
# Agent Hub bridge. Upstream NanoClaw knows nothing about the Oilsand hub
# protocol (hub.go), so passing OILSAND_HUB_* into the container achieves
# nothing on its own — without this process no instance ever appears on the
# channel. Node only, no dependencies; .cjs keeps it CommonJS regardless of
# any package.json "type" nearby.
COPY <<'HUBBRIDGE_EOF' /usr/local/bin/oilsand-hub-bridge.cjs
// Registers this Nanoclaw container on the Oilsand Agent Hub and keeps the
// connection up, so the TUI's Hub tab lists it as a peer. Reconnects with
// backoff; answers direct messages with a short liveness/status line.
const net = require('net');
const os = require('os');

const HOST = process.env.OILSAND_HUB_HOST || '';
const PORT = parseInt(process.env.OILSAND_HUB_PORT || '40117', 10);
const NAME = process.env.OILSAND_HUB_NAME || 'nanoclaw';
const PING_MS = 30000;
const BACKOFF_MAX = 30000;

// No hub configured: exit quietly so the entrypoint stays clean.
if (!HOST) { process.exit(0); }

// The hub may hand back a de-duplicated name (e.g. "nanoclaw-01-2"); track it
// so we can tell which direct messages are addressed to us.
let assigned = NAME;
let backoff = 1000;

function log(msg) {
  console.log('[hub-bridge] ' + new Date().toISOString() + ' ' + msg);
}

function statusText() {
  return assigned + ' is online (NanoClaw container on host ' + os.hostname() +
    '). This is a presence bridge: for agent administration use ncl inside ' +
    'the container (TUI: Agents > Nanoclaw > connect).';
}

function connect() {
  const sock = net.createConnection({ host: HOST, port: PORT });
  let buf = '';
  let ping = null;
  let settled = false;

  const teardown = function () {
    if (settled) { return; }
    settled = true;
    if (ping) { clearInterval(ping); ping = null; }
    log('disconnected; retrying in ' + backoff + 'ms');
    setTimeout(connect, backoff);
    backoff = Math.min(backoff * 2, BACKOFF_MAX);
  };

  sock.on('connect', function () {
    backoff = 1000;
    log('connected to ' + HOST + ':' + PORT + ' as ' + NAME);
    sock.write(JSON.stringify({ type: 'hello', name: NAME, kind: 'agent' }) + '\n');
    ping = setInterval(function () {
      sock.write(JSON.stringify({ type: 'ping' }) + '\n');
    }, PING_MS);
  });

  sock.on('data', function (chunk) {
    buf += chunk.toString('utf8');
    let i;
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i);
      buf = buf.slice(i + 1);
      if (!line.trim()) { continue; }
      let f;
      try { f = JSON.parse(line); } catch (e) { continue; }
      if (f.type === 'welcome') {
        assigned = f.name || NAME;
        log('joined channel as ' + assigned);
      } else if (f.type === 'msg' && f.to === assigned && f.from && f.from !== assigned) {
        sock.write(JSON.stringify({ type: 'msg', to: f.from, text: statusText() }) + '\n');
      }
    }
  });

  sock.on('error', function (err) { log('socket error: ' + err.message); });
  sock.on('close', teardown);
}

connect();
HUBBRIDGE_EOF
# Entrypoint: bring up the inner dockerd, start the hub bridge, then hand off
# to CMD. The cgroup shuffle mirrors the official docker:dind entrypoint —
# on cgroup v2 dockerd can only enable controllers for child cgroups once the
# root cgroup's processes have moved into a leaf.
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
# Join the Agent Hub, if one was configured at deploy time. Backgrounded and
# non-fatal: an unreachable hub must never stop NanoClaw from starting.
if [ -n "${OILSAND_HUB_HOST:-}" ]; then
  echo "[entrypoint] joining Agent Hub at ${OILSAND_HUB_HOST}:${OILSAND_HUB_PORT:-40117} as ${OILSAND_HUB_NAME:-nanoclaw}"
  node /usr/local/bin/oilsand-hub-bridge.cjs >> /var/log/oilsand-hub-bridge.log 2>&1 &
fi
exec "$@"
ENTRYPOINT_EOF
RUN chmod +x /usr/local/bin/nanoclaw-entrypoint.sh
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
// keep adding instances instead of colliding. Each container receives the Agent
// Hub coordinates (OILSAND_HUB_HOST/PORT/NAME), which the bridge baked into the
// image reads on startup to join the shared channel on the gateway (hub.go).
func (m *model) nanoclawDeployScript(instances int) string {
	if instances < 1 {
		instances = 1
	}
	base := strings.TrimRight(m.gateway, "/") + "/olla/openai/v1"
	key := orDefault(m.token, "olla")
	model := m.effDefaultModel()
	hubHost := hostFromURL(m.gateway)
	dfB64 := base64.StdEncoding.EncodeToString([]byte(nanoclawDockerfile))

	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(dockerBootstrap)
	// The Dockerfile is (re)staged on every deploy and the image is rebuilt
	// whenever it no longer matches the Dockerfile it was built from (tracked
	// via a label carrying the Dockerfile's sha256). Without this, a worker
	// bootstrapped by an older TUI keeps its original image forever and every
	// new instance inherits its bugs — e.g. images from before the upgrade
	// marker was baked in crash-loop on NanoClaw's upgrade tripwire. After a
	// rebuild any existing containers are recreated on the new image so the
	// whole worker converges instead of only the instances added today.
	b.WriteString(fmt.Sprintf(`NANO_IMG='%s'
mkdir -p "$HOME/.nanoclaw"
echo %s | base64 -d > "$HOME/.nanoclaw/Dockerfile"
DF_SHA="$(sha256sum "$HOME/.nanoclaw/Dockerfile" | cut -d' ' -f1)"
IMG_SHA="$(sudo docker image inspect "$NANO_IMG" --format '{{index .Config.Labels "%s"}}' 2>/dev/null || true)"
if [ "$IMG_SHA" != "$DF_SHA" ]; then
  echo "[deploy] building $NANO_IMG (image missing or built from an outdated Dockerfile)…"
  sudo docker build --label "%s=$DF_SHA" -t "$NANO_IMG" "$HOME/.nanoclaw"
`+nanoclawRecreateFragment+`fi
`, nanoclawImage, dfB64, nanoclawDockerfileLabel, nanoclawDockerfileLabel))
	b.WriteString(fmt.Sprintf(`started=""
for _i in $(seq 1 %d); do
  n=1
  while sudo docker ps -a --format '{{.Names}}' | grep -qx "nanoclaw-$(printf '%%02d' "$n")"; do n=$((n+1)); done
  NAME="nanoclaw-$(printf '%%02d' "$n")"
  echo "[deploy] starting container $NAME…"
  sudo docker run -d --restart unless-stopped --privileged --name "$NAME" \
    -v "oilsand-$NAME:/opt/nanoclaw/store" \
    -v "oilsand-$NAME-docker:/var/lib/docker" \
    -e OPENAI_BASE_URL='%s' \
    -e OPENAI_API_KEY='%s' \
    -e ANTHROPIC_BASE_URL='%s' \
    -e ANTHROPIC_AUTH_TOKEN='%s' \
    -e NANOCLAW_MODEL='%s' \
    -e OILSAND_HUB_HOST='%s' \
    -e OILSAND_HUB_PORT='%s' \
    -e OILSAND_HUB_NAME="$NAME" \
    "$NANO_IMG" >/dev/null
  started="$started $NAME"
done
echo "[deploy] nanoclaw instances on this worker:"
sudo docker ps --filter name=nanoclaw- --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'
echo "[deploy] started:$started — each instance is isolated in its own container (state volume oilsand-<name>)."
`, instances, shSingle(base), shSingle(key), shSingle(base), shSingle(key), shSingle(model), shSingle(hubHost), hubPort))
	return b.String()
}

// nanoclawShellRC is the bash rc for an in-container session: it puts ncl on
// PATH (for containers from an image built before the symlink existed) and
// prints a short crib sheet, because ncl is not discoverable by itself.
const nanoclawShellRC = `[ -f /etc/bash.bashrc ] && . /etc/bash.bashrc
export PATH="/opt/nanoclaw/bin:$PATH"
export PS1="nanoclaw:\w\$ "
cd /opt/nanoclaw 2>/dev/null || true
echo "NanoClaw container shell. ncl is the admin CLI - each call is one-shot:"
echo "  ncl help              every resource and verb"
echo "  ncl groups list       agent groups"
echo "  ncl sessions list     active sessions"
echo "  ncl tasks list        scheduled tasks"
echo "Logs: docker logs (outside) or /var/log/oilsand-hub-bridge.log (hub bridge)."
echo "exit or ctrl-d returns to the TUI."
`

// nanoclawConnectRemoteCmd opens an interactive shell inside a Nanoclaw
// container.
//
// It deliberately does NOT run ncl directly. ncl is a one-shot admin client:
// it sends a single request frame over the container's control socket
// (data/ncl.sock) and exits, so running it bare as a "session" returns
// immediately and the console looks like it died on open. An interactive bash
// with ncl on PATH is the durable surface, and it also survives NanoClaw
// itself being down — you land in a shell that can show you why.
func nanoclawConnectRemoteCmd(name string) string {
	rc := base64.StdEncoding.EncodeToString([]byte(nanoclawShellRC))
	// The rc file is staged inside the container, so this works on images
	// built before the shell helper existed.
	inner := "echo " + rc + " | base64 -d > /tmp/.oilsand-rc; exec bash --rcfile /tmp/.oilsand-rc -i"
	return "sudo docker exec -it -w /opt/nanoclaw '" + shSingle(name) + "' bash -c \"" + inner + "\""
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
    sudo docker rm -f "$c" >/dev/null || true
    sudo docker run -d --restart unless-stopped --privileged --name "$c" \
      -v "oilsand-$c:/opt/nanoclaw/store" \
      -v "oilsand-$c-docker:/var/lib/docker" \
      --env-file "$ENVF" "$NANO_IMG" >/dev/null
    rm -f "$ENVF"
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
