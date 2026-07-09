package main

// The API front door is a small TLS-terminating nginx layer on the gateway VM
// that gives clients a clean, secured base URL — https://<gateway>/api/v1 —
// in front of Olla's hardcoded /olla/<profile> routes. It validates
// "Authorization: Bearer <key>" against an nginx map and proxies each key to
// its mapped upstream: the whole Olla pool by default, or one specific
// backend (OmniRoute, a Nutanix Enterprise AI endpoint, any OpenAI-compatible
// URL). Olla Community enforces no inbound keys, so this layer is also the
// only real authentication in front of the pool. Installed on demand from the
// Access section (`v`); the key store lives on the gateway as a fully managed
// nginx map file, so the gateway stays the single source of truth.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	neturl "net/url"
	"os/exec"
	"regexp"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	apiGatePort     = "443"
	apiGateConfPath = "/etc/nginx/conf.d/olla-api-gate.conf"
	apiKeysConfPath = "/etc/nginx/conf.d/olla-api-keys.conf"
	// apiGateMarker is echoed by the fetch script when the gate conf exists.
	apiGateMarker = "OILSAND_GATE_ON"
)

// apiKeyEntry is one client API key and the upstream it maps to. Target is a
// full OpenAI-compatible base URL (IP-based: nginx variable proxy_pass would
// need a resolver for hostnames). UpstreamKey, when set, replaces the client's
// Authorization header upstream — for backends like Nutanix Enterprise AI
// that require their own key; empty passes the client header through.
type apiKeyEntry struct {
	Name        string
	Key         string
	Target      string
	UpstreamKey string
}

// newAPIKey mints a client key: sk-oilsand- + 40 hex chars (20 random bytes).
func newAPIKey() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return "sk-oilsand-" + hex.EncodeToString(b)
}

// sanitizeKeyName keeps key names safe inside the nginx conf comments (and
// unambiguous to parse back): letters/digits/._- pass, everything else -> "-".
func sanitizeKeyName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			return r
		}
		return '-'
	}, strings.TrimSpace(s))
}

// poolTarget is the "whole pool" upstream for a key: Olla's OpenAI namespace
// on this same VM, reached over loopback so it needs no resolver and never
// leaves the host.
func poolTarget(gateway string) string {
	port := "40114"
	if u, err := neturl.Parse(gateway); err == nil && u.Port() != "" {
		port = u.Port()
	}
	return "http://127.0.0.1:" + port + "/olla/openai/v1"
}

// endpointTarget converts an Olla endpoint entry into the direct
// OpenAI-compatible base URL for that backend: every supported type (openai,
// vllm, lmstudio — and ollama's compat layer) serves its OpenAI API under
// <url>/v1, including base-path backends like NAI (<host>/api + /v1).
func endpointTarget(e endpointEntry) string {
	return strings.TrimRight(e.URL, "/") + "/v1"
}

// ---- key store (nginx map file on the gateway) ------------------------------

var apiKeyMapLine = regexp.MustCompile(`^\s*"Bearer ([^"]+)"\s+"([^"]*)"\s*;\s*(?:#\s*name=(\S*))?`)

// renderAPIKeysConf renders the fully managed key store: one map resolving a
// Bearer key to its upstream target ("" = reject with 401), and one resolving
// the Authorization header to send upstream (default: pass through).
func renderAPIKeysConf(keys []apiKeyEntry) string {
	var b strings.Builder
	b.WriteString("# Managed by oilsand-tui: API keys for the https://<gateway>/api/v1 front\n")
	b.WriteString("# door. Rewritten on every key change - do not edit by hand.\n")
	b.WriteString("map $http_authorization $olla_api_target {\n")
	b.WriteString("    default \"\";\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "    \"Bearer %s\" \"%s\"; # name=%s\n", k.Key, k.Target, sanitizeKeyName(k.Name))
	}
	b.WriteString("}\n")
	b.WriteString("map $http_authorization $olla_api_upstream_auth {\n")
	b.WriteString("    default $http_authorization;\n")
	for _, k := range keys {
		if k.UpstreamKey != "" {
			fmt.Fprintf(&b, "    \"Bearer %s\" \"Bearer %s\"; # name=%s\n", k.Key, k.UpstreamKey, sanitizeKeyName(k.Name))
		}
	}
	b.WriteString("}\n")
	return b.String()
}

// parseAPIKeysConf reads the key store back out of the managed conf text.
func parseAPIKeysConf(text string) []apiKeyEntry {
	var keys []apiKeyEntry
	section := ""
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.Contains(line, "$olla_api_target {"):
			section = "target"
			continue
		case strings.Contains(line, "$olla_api_upstream_auth {"):
			section = "auth"
			continue
		case strings.TrimSpace(line) == "}":
			section = ""
			continue
		}
		mm := apiKeyMapLine.FindStringSubmatch(line)
		if mm == nil {
			continue
		}
		switch section {
		case "target":
			keys = append(keys, apiKeyEntry{Key: mm[1], Target: mm[2], Name: mm[3]})
		case "auth":
			for i := range keys {
				if keys[i].Key == mm[1] {
					keys[i].UpstreamKey = strings.TrimPrefix(mm[2], "Bearer ")
				}
			}
		}
	}
	return keys
}

// ---- install script ----------------------------------------------------------

// apiGateInstallScript is the idempotent gateway-side installer: nginx, a
// one-time self-signed cert, the SELinux boolean nginx needs to proxy to
// upstreams on Rocky, the static server block, an empty key store on first
// install, service enable + reload, and the firewalld opening.
func apiGateInstallScript() string {
	var b strings.Builder
	b.WriteString(`set -e
echo "[api-gate] ensuring nginx is installed…"
command -v nginx >/dev/null 2>&1 || sudo dnf install -y nginx
if [ ! -f /etc/nginx/olla-api.crt ]; then
  echo "[api-gate] generating self-signed TLS certificate…"
  IP=$(hostname -I 2>/dev/null | cut -d" " -f1)
  sudo openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -subj "/CN=${IP:-olla-gateway}" \
    -keyout /etc/nginx/olla-api.key -out /etc/nginx/olla-api.crt >/dev/null 2>&1
  sudo chmod 600 /etc/nginx/olla-api.key
fi
# Allow nginx to open outbound connections to Olla/backends under SELinux.
sudo setsebool -P httpd_can_network_connect 1 2>/dev/null || true
echo "[api-gate] writing ` + apiGateConfPath + `…"
sudo tee ` + apiGateConfPath + ` >/dev/null <<'CONFEOF'
# Managed by oilsand-tui: TLS + API-key front door for the LLM gateway.
# Clients call https://<gateway>/api/v1/... with "Authorization: Bearer <key>";
# each key maps to an upstream target in olla-api-keys.conf.
server {
    listen ` + apiGatePort + ` ssl;
    server_name _;
    ssl_certificate     /etc/nginx/olla-api.crt;
    ssl_certificate_key /etc/nginx/olla-api.key;

    location = /api/v1 { return 302 /api/v1/; }

    location ~ ^/api/v1(/.*)$ {
        if ($olla_api_target = "") {
            return 401 '{"error":{"message":"invalid or missing API key","type":"invalid_request_error"}}';
        }
        proxy_http_version 1.1;
        # No buffering: chat completions stream as SSE.
        proxy_buffering off;
        proxy_read_timeout 900s;
        proxy_send_timeout 900s;
        proxy_set_header Authorization $olla_api_upstream_auth;
        proxy_pass $olla_api_target$1$is_args$args;
    }
}
CONFEOF
if [ ! -f ` + apiKeysConfPath + ` ]; then
  echo "[api-gate] creating empty key store…"
  sudo tee ` + apiKeysConfPath + ` >/dev/null <<'KEYSEOF'
` + strings.TrimRight(renderAPIKeysConf(nil), "\n") + `
KEYSEOF
fi
sudo systemctl enable --now nginx
sudo nginx -t
sudo systemctl reload nginx
if command -v firewall-cmd >/dev/null 2>&1 && sudo systemctl is-active --quiet firewalld; then
  echo "[api-gate] opening firewall port ` + apiGatePort + `/tcp"
  sudo firewall-cmd --permanent --add-port=` + apiGatePort + `/tcp >/dev/null 2>&1 || true
  sudo firewall-cmd --reload >/dev/null 2>&1 || true
fi
IP=$(hostname -I 2>/dev/null | cut -d" " -f1)
echo "[api-gate] self-signed cert (import or use curl -k): /etc/nginx/olla-api.crt"
echo "[api-gate] front door ready: https://${IP:-<gateway>}/api/v1"
`)
	return b.String()
}

// ---- commands ----------------------------------------------------------------

// apiGateMsg reports the outcome of installing the front door.
type apiGateMsg struct {
	err error
	out string
}

// apiKeysMsg carries the gateway's key store state (and whether the front
// door is installed at all).
type apiKeysMsg struct {
	on    bool
	keys  []apiKeyEntry
	saved bool // true when this is the result of a key-store write
	err   error
}

// runOnGateway runs a script on the gateway host: locally when the gateway is
// this machine (local Olla install), else over SSH.
func runOnGateway(host, user, pass, script string) (string, error) {
	if isLocalHost(host) {
		out, err := exec.Command("bash", "-c", script).CombinedOutput()
		return string(out), err
	}
	if pass == "" {
		return "", fmt.Errorf("no SSH password configured")
	}
	client, err := dialSSH(host, user, pass)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return runSSH(client, script)
}

// installAPIGateCmd installs/refreshes the front door on the gateway.
func installAPIGateCmd(host, user, pass string) tea.Cmd {
	return func() tea.Msg {
		out, err := runOnGateway(host, user, pass, apiGateInstallScript())
		if err != nil {
			return apiGateMsg{err: fmt.Errorf("%v: %s", err, lastNonEmptyLine(out))}
		}
		return apiGateMsg{out: lastNonEmptyLine(out)}
	}
}

// fetchAPIKeysCmd reads the front door state and key store off the gateway.
func fetchAPIKeysCmd(host, user, pass string) tea.Cmd {
	script := "if [ -f " + apiGateConfPath + " ]; then echo " + apiGateMarker + "; fi; " +
		"cat " + apiKeysConfPath + " 2>/dev/null || true"
	return func() tea.Msg {
		out, err := runOnGateway(host, user, pass, script)
		if err != nil {
			return apiKeysMsg{err: err}
		}
		return apiKeysMsg{
			on:   strings.Contains(out, apiGateMarker),
			keys: parseAPIKeysConf(out),
		}
	}
}

// saveAPIKeysCmd writes the full key store to the gateway and reloads nginx
// (reload, not restart: in-flight streams survive; revoked keys 401 at once).
func saveAPIKeysCmd(host, user, pass string, keys []apiKeyEntry) tea.Cmd {
	b64 := base64.StdEncoding.EncodeToString([]byte(renderAPIKeysConf(keys)))
	script := "echo " + b64 + " | base64 -d | sudo tee " + apiKeysConfPath + " >/dev/null" +
		" && sudo nginx -t && sudo systemctl reload nginx"
	return func() tea.Msg {
		if out, err := runOnGateway(host, user, pass, script); err != nil {
			return apiKeysMsg{err: fmt.Errorf("%v: %s", err, lastNonEmptyLine(out))}
		}
		return apiKeysMsg{on: true, keys: keys, saved: true}
	}
}
