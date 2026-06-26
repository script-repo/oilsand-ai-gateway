package main

import (
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

// endpointEntry is one static discovery endpoint in olla.yaml.
type endpointEntry struct {
	URL           string `yaml:"url"`
	Name          string `yaml:"name"`
	Type          string `yaml:"type"`
	Priority      int    `yaml:"priority"`
	CheckInterval string `yaml:"check_interval,omitempty"`
	CheckTimeout  string `yaml:"check_timeout,omitempty"`
}

func dialSSH(host, user, password string) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         20 * time.Second,
	}
	addr := host
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(host, "22")
	}
	return ssh.Dial("tcp", addr, cfg)
}

func runSSH(client *ssh.Client, cmd string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

func readRemoteFile(client *ssh.Client, path string) (string, error) {
	return runSSH(client, "cat "+path)
}

// sshWriteFile writes content to remotePath on host (base64-encoded so no shell
// quoting is needed). If mkdir is non-empty that directory is created first; if
// makeExec is true the file is chmod +x. Paths may use ~ (expanded remotely).
func sshWriteFile(host, user, password, mkdir, remotePath, content string, makeExec bool) error {
	client, err := dialSSH(host, user, password)
	if err != nil {
		return err
	}
	defer client.Close()
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	var cmd string
	if mkdir != "" {
		cmd = "mkdir -p " + mkdir + " && "
	}
	cmd += fmt.Sprintf("echo %s | base64 -d > %s", b64, remotePath)
	if makeExec {
		cmd += " && chmod +x " + remotePath
	}
	if out, err := runSSH(client, cmd); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// uploadRemoteScript stages an executable script (run later in a login shell).
func uploadRemoteScript(host, user, password, remotePath, script string) error {
	return sshWriteFile(host, user, password, "", remotePath, script, true)
}

// crushMergePy deep-merges a JSON fragment (argv[1]) into the existing
// ~/.config/crush/crush.json, recursing into nested objects so the TUI's olla
// provider update preserves sections it does not manage (notably manually
// registered `mcp` servers, plus `lsp`/`options`). Lists and scalars are
// replaced wholesale (so the olla model list updates cleanly).
const crushMergePy = `import json, os, sys
base = os.path.expanduser("~/.config/crush/crush.json")
os.makedirs(os.path.dirname(base), exist_ok=True)
with open(sys.argv[1]) as f:
    new = json.load(f)
cur = {}
if os.path.exists(base):
    try:
        with open(base) as f:
            cur = json.load(f)
    except Exception:
        cur = {}
def merge(a, b):
    for k, v in b.items():
        if isinstance(v, dict) and isinstance(a.get(k), dict):
            merge(a[k], v)
        else:
            a[k] = v
merge(cur, new)
with open(base, "w") as f:
    json.dump(cur, f, indent=2)
    f.write("\n")
`

// sshMergeCrushConfig merges the given crush.json document (TUI-managed
// providers + $schema) into the existing remote config instead of overwriting
// it, so user-added sections (e.g. an `mcp` server registration) survive a Crush
// open/redeploy from the TUI.
func sshMergeCrushConfig(host, user, password, config string) error {
	client, err := dialSSH(host, user, password)
	if err != nil {
		return err
	}
	defer client.Close()
	cfgB64 := base64.StdEncoding.EncodeToString([]byte(config))
	pyB64 := base64.StdEncoding.EncodeToString([]byte(crushMergePy))
	cmd := fmt.Sprintf("mkdir -p ~/.config/crush && echo %s | base64 -d > /tmp/crush-new.json && echo %s | base64 -d > /tmp/crush-merge.py && python3 /tmp/crush-merge.py /tmp/crush-new.json && rm -f /tmp/crush-merge.py /tmp/crush-new.json",
		cfgB64, pyB64)
	if out, err := runSSH(client, cmd); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// mutateEndpoints applies a transform to the discovery.static.endpoints list in
// the olla.yaml document and returns the new YAML text.
func mutateEndpoints(current string, fn func([]endpointEntry) []endpointEntry) (string, error) {
	var doc map[string]any
	if strings.TrimSpace(current) != "" {
		if err := yaml.Unmarshal([]byte(current), &doc); err != nil {
			return "", err
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	disc, _ := doc["discovery"].(map[string]any)
	if disc == nil {
		disc = map[string]any{}
	}
	static, _ := disc["static"].(map[string]any)
	if static == nil {
		static = map[string]any{}
	}

	var eps []endpointEntry
	if rawList, ok := static["endpoints"].([]any); ok {
		b, _ := yaml.Marshal(rawList)
		_ = yaml.Unmarshal(b, &eps)
	}
	eps = fn(eps)

	static["endpoints"] = eps
	disc["static"] = static
	doc["discovery"] = disc

	ensureGatewayDefaults(doc)

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// readEndpointsFromYAML parses the discovery.static.endpoints list out of an
// olla.yaml document.
func readEndpointsFromYAML(current string) []endpointEntry {
	var doc map[string]any
	if strings.TrimSpace(current) == "" {
		return nil
	}
	if err := yaml.Unmarshal([]byte(current), &doc); err != nil {
		return nil
	}
	disc, _ := doc["discovery"].(map[string]any)
	static, _ := disc["static"].(map[string]any)
	var eps []endpointEntry
	if rawList, ok := static["endpoints"].([]any); ok {
		b, _ := yaml.Marshal(rawList)
		_ = yaml.Unmarshal(b, &eps)
	}
	return eps
}

// ReadEndpoints SSHes to the gateway and returns its configured endpoints.
func ReadEndpoints(host, user, password string) ([]endpointEntry, error) {
	client, err := dialSSH(host, user, password)
	if err != nil {
		return nil, fmt.Errorf("ssh connect failed: %w", err)
	}
	defer client.Close()
	current, err := readRemoteFile(client, "/etc/olla/olla.yaml")
	if err != nil {
		return nil, err
	}
	return readEndpointsFromYAML(current), nil
}

// ApplyEndpointChange reads olla.yaml on the gateway, applies fn, writes it back
// and restarts Olla. Returns a human-readable status message.
func ApplyEndpointChange(host, user, password string, fn func([]endpointEntry) []endpointEntry) (string, error) {
	client, err := dialSSH(host, user, password)
	if err != nil {
		return "", fmt.Errorf("ssh connect failed: %w", err)
	}
	defer client.Close()

	current, _ := readRemoteFile(client, "/etc/olla/olla.yaml")
	newText, err := mutateEndpoints(current, fn)
	if err != nil {
		return "", fmt.Errorf("rewrite config: %w", err)
	}

	// Write via a heredoc to /tmp then install with correct ownership.
	writeCmd := fmt.Sprintf("cat > /tmp/olla.yaml.new <<'OLLAEOF'\n%s\nOLLAEOF", newText)
	if out, err := runSSH(client, writeCmd); err != nil {
		return "", fmt.Errorf("write temp config: %v (%s)", err, strings.TrimSpace(out))
	}
	applyCmd := "sudo install -o olla -g olla -m 0644 /tmp/olla.yaml.new /etc/olla/olla.yaml && sudo systemctl restart olla"
	if out, err := runSSH(client, applyCmd); err != nil {
		return "", fmt.Errorf("apply/restart failed: %v (%s)", err, strings.TrimSpace(out))
	}
	return "endpoint list updated and Olla restarted", nil
}

// RestartOlla restarts the Olla service on the gateway. Olla re-scans every
// endpoint's installed models on startup, so this makes a just-downloaded model
// routable immediately instead of waiting for the periodic discovery interval
// (Olla v0.0.28 exposes no discovery-refresh API).
func RestartOlla(host, user, password string) error {
	client, err := dialSSH(host, user, password)
	if err != nil {
		return fmt.Errorf("ssh connect failed: %w", err)
	}
	defer client.Close()
	if out, err := runSSH(client, "sudo systemctl restart olla"); err != nil {
		return fmt.Errorf("restart olla: %v (%s)", err, strings.TrimSpace(out))
	}
	return nil
}

// ensureGatewayDefaults makes the gateway behave for real-world LLM traffic:
//   - spreads load across workers (least-connections instead of Olla's default
//     "priority", which pins every request to the first endpoint), and
//   - raises the stock 30s timeouts so slow, CPU-bound model loads and long
//     prompt evaluations don't get cut off with a 502 (the "request timeout
//     after 30s" error). Existing operator-set values are left untouched.
func ensureGatewayDefaults(doc map[string]any) {
	proxy, _ := doc["proxy"].(map[string]any)
	if proxy == nil {
		proxy = map[string]any{}
	}
	if eng, _ := proxy["engine"].(string); eng == "" {
		proxy["engine"] = "sherpa"
	}
	if lb, _ := proxy["load_balancer"].(string); lb == "" || lb == "priority" {
		proxy["load_balancer"] = "least-connections"
	}
	setIfMissing(proxy, "connection_timeout", "60s")
	setIfMissing(proxy, "response_timeout", "900s")
	setIfMissing(proxy, "read_timeout", "900s")
	// response_header_timeout defaults to a hardcoded 30s in Olla; cold model
	// loads on CPU-only workers send no header byte until the model is resident,
	// so the first request 502s ("LLM model taking longer than expected") unless
	// this is raised. Configurable since Olla v0.0.17 (issue #163 / PR #165).
	setIfMissing(proxy, "response_header_timeout", "900s")
	doc["proxy"] = proxy

	server, _ := doc["server"].(map[string]any)
	if server == nil {
		server = map[string]any{}
	}
	setIfMissing(server, "read_timeout", "900s")
	setIfMissing(server, "write_timeout", "0s") // 0 = no limit; required for streaming
	setIfMissing(server, "idle_timeout", "900s")
	doc["server"] = server
}

func setIfMissing(m map[string]any, key string, val any) {
	if _, ok := m[key]; !ok {
		m[key] = val
	}
}

func addEndpointFn(e endpointEntry) func([]endpointEntry) []endpointEntry {
	return func(eps []endpointEntry) []endpointEntry {
		out := eps[:0]
		for _, x := range eps {
			if x.Name != e.Name {
				out = append(out, x)
			}
		}
		return append(out, e)
	}
}

func removeEndpointFn(name string) func([]endpointEntry) []endpointEntry {
	return func(eps []endpointEntry) []endpointEntry {
		var out []endpointEntry
		for _, x := range eps {
			if x.Name != name {
				out = append(out, x)
			}
		}
		return out
	}
}
