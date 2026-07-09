package main

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

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
	// omniroutePort serves both the OpenAI-compatible API (/v1) and the web
	// dashboard; it is the upstream default.
	omniroutePort = "20128"
)

// omnirouteRunFragment (re)creates the single omniroute service container from
// $IMG, replacing any previous one so repeated deploys/updates converge on the
// running image. The named volume omniroute-data keeps the SQLite database and
// provider/key config across recreations (upstream persists to /app/data).
// Expects $IMG to be set and contains no fmt verbs, so it concatenates into
// Sprintf formats and plain builders alike.
const omnirouteRunFragment = `sudo docker rm -f omniroute >/dev/null 2>&1 || true
sudo docker run -d --name omniroute --restart unless-stopped --stop-timeout 40 \
  -p ` + omniroutePort + `:` + omniroutePort + ` -v omniroute-data:/app/data "$IMG" >/dev/null
sudo docker ps --filter name=omniroute --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
`

// omnirouteDeployScript installs Docker on the worker (idempotent), pulls the
// OmniRoute image, and starts it as a detached service exposing the dashboard +
// OpenAI-compatible API on omniroutePort. It ends by printing the reachable URL
// (the worker's own primary IP) so the operator can open it to add providers.
func (m *model) omnirouteDeployScript() string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(dockerBootstrap)
	b.WriteString("IMG='" + omnirouteImage + "'\n")
	b.WriteString(`echo "[deploy] pulling $IMG…"
sudo docker pull "$IMG"
echo "[deploy] starting omniroute service…"
`)
	b.WriteString(omnirouteRunFragment)
	b.WriteString(`IP=$(hostname -I 2>/dev/null | cut -d" " -f1)
echo "[deploy] OmniRoute is up — dashboard + OpenAI-compatible API at http://${IP:-<this-host>}:` + omniroutePort + `"
echo "[deploy] open that URL to add providers/keys. You can close this window."
`)
	return b.String()
}

// omnirouteUpdateScript pulls the latest OmniRoute image and recreates the
// container on it; the named data volume preserves configuration across the
// swap. Shared by the Update section (agents / update-all).
func omnirouteUpdateScript() string {
	return "echo '[update] updating omniroute'\nIMG='" + omnirouteImage + "'\n" +
		"sudo docker pull \"$IMG\"\n" + omnirouteRunFragment
}

// omnirouteConsoleScript is what "open" runs for OmniRoute: print the reachable
// dashboard/API URL, show container status, then follow the service logs (it is
// a background service, not an interactive CLI). Written without single quotes
// so loginShell can wrap it in `bash -lc '…'`.
func omnirouteConsoleScript() string {
	return `IP=$(hostname -I 2>/dev/null | cut -d" " -f1)
echo "OmniRoute dashboard + OpenAI-compatible API: http://${IP:-localhost}:` + omniroutePort + `"
sudo docker ps -a --filter name=omniroute --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
echo "[following omniroute logs - Ctrl-C to exit]"
sudo docker logs -f --tail 50 omniroute`
}

// removeOmnirouteEndpointsFn drops the Olla endpoint(s) that represent OmniRoute
// from the discovery list: entries carrying a managed OmniRoute name ("omniroute"
// or the per-host "omniroute-<host>" fallback), plus any openai endpoint pointing
// at one of the given hosts on omniroutePort. It only ever removes OmniRoute's own
// entries; Ollama worker endpoints are left untouched.
func removeOmnirouteEndpointsFn(hosts []string) func([]endpointEntry) []endpointEntry {
	names := map[string]bool{"omniroute": true}
	hostSet := map[string]bool{}
	for _, h := range hosts {
		names["omniroute-"+sanitizeHostLabel(h)] = true
		hostSet[h] = true
	}
	return func(eps []endpointEntry) []endpointEntry {
		var out []endpointEntry
		for _, e := range eps {
			if names[e.Name] {
				continue
			}
			if e.Type == "openai" && hostSet[hostFromURL(e.URL)] && strings.Contains(e.URL, ":"+omniroutePort) {
				continue
			}
			out = append(out, e)
		}
		return out
	}
}

// deregisterOmnirouteCmd removes OmniRoute's own Olla endpoint(s) from the gateway
// once the service is fully removed, so a dead openai endpoint isn't left behind
// for Olla to health-check. Best-effort: the outcome is surfaced via a notice.
func deregisterOmnirouteCmd(gwHost, user, pass string, hosts []string) tea.Cmd {
	return func() tea.Msg {
		if _, err := ApplyEndpointChange(gwHost, user, pass, removeOmnirouteEndpointsFn(hosts)); err != nil {
			return notifyMsg("omniroute endpoint left in place: " + err.Error())
		}
		return notifyMsg("omniroute endpoint de-registered from the gateway")
	}
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
