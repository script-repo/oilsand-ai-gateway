package main

import "strings"

// OmniRoute (https://github.com/diegosouzapw/OmniRoute) is a free AI gateway:
// a single OpenAI-compatible endpoint that fans out to 200+ upstream providers
// with automatic failover and prompt compression. It ships as a Node.js app but
// is deployed here from the official Docker image as one long-running service
// container on a chosen worker — a "worker node running OmniRoute" that sits
// alongside the Ollama pool. Unlike Nanoclaw it is a single service (not many
// isolated agent sandboxes), so it deploys through the ordinary host-install
// path and is special-cased by name where its Docker lifecycle differs.

const (
	omnirouteImage = "diegosouzapw/omniroute:latest"
	// omniroutePort serves the dashboard; API and live-WS use sibling ports.
	omniroutePort    = "20128"
	omnirouteAPIPort = "20129"
	omnirouteWSPort  = "20132"
	omnirouteEnvPath = "/opt/oilsand/omniroute.env"
	omnirouteNet     = "omniroute-net"
)

// omnirouteEnvFragment writes a persistent env file with production secrets
// when missing. Upstream requires OMNIROUTE_WS_BRIDGE_SECRET (and related keys)
// in Docker; without them the process often dies immediately after start.
const omnirouteEnvFragment = `ENVF='` + omnirouteEnvPath + `'
sudo mkdir -p /opt/oilsand
if [ ! -f "$ENVF" ]; then
  echo "[deploy] writing $ENVF (first-time secrets)…"
  umask 077
  {
    echo "OMNIROUTE_WS_BRIDGE_SECRET=$(openssl rand -base64 32 | tr -d '\n')"
    echo "JWT_SECRET=$(openssl rand -hex 32)"
    echo "API_KEY_SECRET=$(openssl rand -hex 32)"
    echo "STORAGE_ENCRYPTION_KEY=$(openssl rand -hex 32)"
    echo "STORAGE_ENCRYPTION_KEY_VERSION=v1"
    echo "MACHINE_ID_SALT=$(openssl rand -hex 16)"
    echo "INITIAL_PASSWORD=oilsand-$(openssl rand -hex 4)"
    echo "PORT=` + omniroutePort + `"
    echo "DASHBOARD_PORT=` + omniroutePort + `"
    echo "API_PORT=` + omnirouteAPIPort + `"
    echo "LIVE_WS_PORT=` + omnirouteWSPort + `"
    echo "HOSTNAME=0.0.0.0"
    echo "DATA_DIR=/app/data"
    echo "NODE_ENV=production"
    echo "REDIS_URL=redis://omniroute-redis:6379"
  } | sudo tee "$ENVF" >/dev/null
  sudo chmod 600 "$ENVF"
  echo "[deploy] initial dashboard password is in $ENVF (INITIAL_PASSWORD)"
fi
`

// omnirouteRunFragment (re)creates redis + the omniroute service on a shared
// network. Expects $IMG to be set. Verifies the app container is running and
// dumps logs on failure so the TUI surfaces a real cause (not bare exit 1).
const omnirouteRunFragment = `sudo docker network create ` + omnirouteNet + ` >/dev/null 2>&1 || true
sudo docker rm -f omniroute-redis >/dev/null 2>&1 || true
sudo docker run -d --name omniroute-redis --restart unless-stopped \
  --network ` + omnirouteNet + ` \
  -v omniroute-redis-data:/data \
  redis:7-alpine redis-server --save 60 1 --loglevel warning >/dev/null
sudo docker rm -f omniroute >/dev/null 2>&1 || true
sudo docker run -d --name omniroute --restart unless-stopped --stop-timeout 40 \
  --network ` + omnirouteNet + ` \
  --env-file ` + omnirouteEnvPath + ` \
  -e REDIS_URL=redis://omniroute-redis:6379 \
  -p ` + omniroutePort + `:` + omniroutePort + ` \
  -p ` + omnirouteAPIPort + `:` + omnirouteAPIPort + ` \
  -p ` + omnirouteWSPort + `:` + omnirouteWSPort + ` \
  -v omniroute-data:/app/data \
  "$IMG" >/dev/null
# Give the entrypoint a moment, then require a live container.
sleep 3
if [ "$(sudo docker inspect -f '{{.State.Running}}' omniroute 2>/dev/null || echo false)" != "true" ]; then
  echo "[deploy] ERROR: omniroute container is not running — recent logs:" >&2
  sudo docker logs --tail 80 omniroute >&2 || true
  exit 1
fi
sudo docker ps --filter name=omniroute --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
`

// omnirouteDeployScript installs Docker on the worker (idempotent), writes
// secrets, pulls the OmniRoute image, and starts redis + the gateway service.
func (m *model) omnirouteDeployScript() string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(dockerBootstrap)
	b.WriteString(omnirouteEnvFragment)
	b.WriteString("IMG='" + omnirouteImage + "'\n")
	b.WriteString(`echo "[deploy] pulling $IMG…"
sudo docker pull "$IMG"
echo "[deploy] starting omniroute-redis + omniroute…"
`)
	b.WriteString(omnirouteRunFragment)
	b.WriteString(`IP=$(hostname -I 2>/dev/null | cut -d" " -f1)
echo "[deploy] OmniRoute is up — dashboard at http://${IP:-<this-host>}:` + omniroutePort + `"
echo "[deploy] OpenAI-compatible API also on :` + omnirouteAPIPort + ` (see docs). You can close this window."
`)
	return b.String()
}

// omnirouteUpdateScript pulls the latest OmniRoute image and recreates the
// containers; named volumes preserve configuration across the swap.
func omnirouteUpdateScript() string {
	return "set -e\n" +
		"echo '[update] updating omniroute'\n" +
		omnirouteEnvFragment +
		"IMG='" + omnirouteImage + "'\n" +
		"sudo docker pull \"$IMG\"\n" + omnirouteRunFragment
}

// omnirouteConsoleScript is what "open" runs for OmniRoute: print the reachable
// dashboard/API URL, show container status, then follow the service logs.
func omnirouteConsoleScript() string {
	return `IP=$(hostname -I 2>/dev/null | cut -d" " -f1)
echo "OmniRoute dashboard: http://${IP:-localhost}:` + omniroutePort + `"
echo "OpenAI-compatible API: http://${IP:-localhost}:` + omnirouteAPIPort + `"
sudo docker ps -a --filter name=omniroute --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
echo "[following omniroute logs - Ctrl-C to exit]"
sudo docker logs -f --tail 50 omniroute`
}

// omnirouteHosts returns every worker OmniRoute was deployed on, falling back to
// the single most-recent registration for configs saved before host lists.
func (m *model) omnirouteHosts() []string {
	if hs := m.agentHosts["OmniRoute"]; len(hs) > 0 {
		return hs
	}
	if h := m.agentReg["OmniRoute"]; h != "" {
		return []string{h}
	}
	return nil
}
