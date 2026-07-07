package main

import (
	"encoding/base64"
	"fmt"
	"strings"

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

// nanoclawDockerfile builds the per-worker Nanoclaw image from the upstream
// project. Upstream is TypeScript managed with pnpm ("start" runs
// dist/index.js), so the image must compile it — pnpm install + pnpm run build
// — before the runtime CMD. The store directory is a volume so each container
// instance keeps its own persistent state.
const nanoclawDockerfile = `# Nanoclaw agent image, built by the Oilsand AI Gateway TUI.
# Every deployed instance is a separate container from this one image.
FROM node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
# Upstream pins pnpm via the packageManager field; corepack provides it.
RUN corepack enable || npm install -g pnpm
RUN git clone --depth 1 https://github.com/qwibitai/nanoclaw /opt/nanoclaw
WORKDIR /opt/nanoclaw
RUN pnpm install --frozen-lockfile || pnpm install
RUN pnpm run build
VOLUME /opt/nanoclaw/store
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
// keep adding instances instead of colliding.
func (m *model) nanoclawDeployScript(instances int) string {
	if instances < 1 {
		instances = 1
	}
	base := strings.TrimRight(m.gateway, "/") + "/olla/openai/v1"
	key := orDefault(m.token, "olla")
	model := m.effDefaultModel()
	dfB64 := base64.StdEncoding.EncodeToString([]byte(nanoclawDockerfile))

	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(dockerBootstrap)
	b.WriteString(fmt.Sprintf(`NANO_IMG='%s'
if ! sudo docker image inspect "$NANO_IMG" >/dev/null 2>&1; then
  echo "[deploy] building $NANO_IMG (first run on this worker — pulls the node base image)…"
  mkdir -p "$HOME/.nanoclaw"
  echo %s | base64 -d > "$HOME/.nanoclaw/Dockerfile"
  sudo docker build -t "$NANO_IMG" "$HOME/.nanoclaw"
fi
`, nanoclawImage, dfB64))
	b.WriteString(fmt.Sprintf(`started=""
for _i in $(seq 1 %d); do
  n=1
  while sudo docker ps -a --format '{{.Names}}' | grep -qx "nanoclaw-$(printf '%%02d' "$n")"; do n=$((n+1)); done
  NAME="nanoclaw-$(printf '%%02d' "$n")"
  echo "[deploy] starting container $NAME…"
  sudo docker run -d --restart unless-stopped --name "$NAME" \
    -v "oilsand-$NAME:/opt/nanoclaw/store" \
    -e OPENAI_BASE_URL='%s' \
    -e OPENAI_API_KEY='%s' \
    -e ANTHROPIC_BASE_URL='%s' \
    -e ANTHROPIC_AUTH_TOKEN='%s' \
    -e NANOCLAW_MODEL='%s' \
    "$NANO_IMG" >/dev/null
  started="$started $NAME"
done
echo "[deploy] nanoclaw instances on this worker:"
sudo docker ps --filter name=nanoclaw- --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'
echo "[deploy] started:$started — each instance is isolated in its own container (state volume oilsand-<name>)."
`, instances, shSingle(base), shSingle(key), shSingle(base), shSingle(key), shSingle(model)))
	return b.String()
}

// nanoclawOpenScript lists the worker's Nanoclaw containers and follows the
// logs of the most recent running instance (Nanoclaw runs as a service, so logs
// are its interactive surface).
const nanoclawOpenScript = `echo "[nanoclaw] instances on this worker:"
sudo docker ps -a --filter name=nanoclaw- --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}' 2>/dev/null || {
  echo "[nanoclaw] docker is not installed here — deploy Nanoclaw first (d in the Agents tab)."
  exit 0
}
LATEST="$(sudo docker ps --filter name=nanoclaw- --format '{{.Names}}' | sort | tail -n 1)"
if [ -z "$LATEST" ]; then
  echo "[nanoclaw] no running instances — deploy one from the Agents tab (d)."
  exit 0
fi
echo "[nanoclaw] following logs of $LATEST (ctrl+c to detach)…"
sudo docker logs -f --tail 40 "$LATEST"
`

// nanoclawUpdateScript rebuilds the image against the latest upstream and
// restarts the containers. Newly deployed instances pick up the fresh image.
func nanoclawUpdateScript() string {
	return fmt.Sprintf(`echo '[update] rebuilding nanoclaw image'
if [ -f "$HOME/.nanoclaw/Dockerfile" ]; then
  sudo docker build --pull --no-cache -t '%s' "$HOME/.nanoclaw" || echo '[update] rebuild failed — keeping current image'
fi
for c in $(sudo docker ps -a --format '{{.Names}}' | grep '^nanoclaw-' || true); do
  echo "[update] restarting $c"
  sudo docker restart "$c" >/dev/null || true
done
sudo docker ps --filter name=nanoclaw- --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'`, nanoclawImage)
}

// nanoclawOpenCmd stages the open script on the worker and returns a
// consoleReadyMsg that runs it interactively (no registration on exit).
func nanoclawOpenCmd(user, host, pass string) tea.Cmd {
	return func() tea.Msg {
		u := orDefault(user, "rocky")
		if host == "" {
			return notifyMsg("no target host for Nanoclaw")
		}
		if pass != "" {
			_, _ = EnsureKeyAuth(host, u, pass)
		}
		const remotePath = "~/.oilsand-nanoclaw-open.sh"
		if err := uploadRemoteScript(host, u, pass, remotePath, nanoclawOpenScript); err != nil {
			return notifyMsg("Nanoclaw open prep failed: " + err.Error())
		}
		return consoleReadyMsg{user: u, host: host, key: managedKeyPath(), cmd: "bash -l " + remotePath, label: "Nanoclaw"}
	}
}

// localNanoclawOpenCmd is nanoclawOpenCmd for a worker that is this machine.
func localNanoclawOpenCmd() tea.Cmd {
	return func() tea.Msg {
		abs, err := localWriteScript("~/.oilsand-nanoclaw-open.sh", nanoclawOpenScript)
		if err != nil {
			return notifyMsg("Nanoclaw open prep failed: " + err.Error())
		}
		return consoleReadyMsg{local: true, cmd: "bash -l " + abs, label: "Nanoclaw"}
	}
}
