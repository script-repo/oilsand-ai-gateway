package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// agentDef describes a terminal AI agent CLI the TUI can launch (and optionally
// deploy) over SSH on one of the managed hosts.
type agentDef struct {
	name       string
	cli        string // command to (re)launch an existing install
	deployable bool   // whether the TUI can install it
	container  bool   // runs as Docker container(s) on the host (multi-instance)
	target     string // "gateway" or "worker"
	endpoint   string // how it reaches models (informational)
	desc       string
}

// depsBootstrap installs the prerequisites the npm/Node agents need on
// Rocky/RHEL and makes `npm -g` usable as a non-root user:
//   - git (Hermes/OpenClaw installers require it)
//   - Node.js 22 via NodeSource (these are Node tools)
//   - a user-writable npm global prefix (~/.npm-global), because NodeSource sets
//     the prefix to /usr, which makes `npm install -g openclaw` fail with EACCES
//     for a normal user. We also persist the bin dir on PATH for future shells.
const depsBootstrap = `echo "[deploy] ensuring git…"
command -v git >/dev/null 2>&1 || sudo dnf install -y git
if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
  echo "[deploy] installing Node.js 22 + npm via NodeSource (sudo)…"
  curl -fsSL https://rpm.nodesource.com/setup_22.x | sudo -E bash -
  sudo dnf install -y nodejs
fi
NPM_PREFIX="$HOME/.npm-global"
mkdir -p "$NPM_PREFIX/bin"
npm config set prefix "$NPM_PREFIX" >/dev/null 2>&1 || true
case ":$PATH:" in *":$NPM_PREFIX/bin:"*) ;; *) export PATH="$NPM_PREFIX/bin:$PATH";; esac
grep -q '.npm-global/bin' "$HOME/.bashrc" 2>/dev/null || echo 'export PATH="$HOME/.npm-global/bin:$PATH"' >> "$HOME/.bashrc"
`

// crushDeployScript installs crush from the Charm yum repo on the Olla server
// and launches it (the Olla provider config is written separately).
const crushDeployScript = `set -e
if ! command -v crush >/dev/null 2>&1; then
  echo "[deploy] adding Charm repo and installing crush (sudo)…"
  sudo rpm --import https://repo.charm.sh/yum/gpg.key || true
  printf '[charm]\nname=Charm\nbaseurl=https://repo.charm.sh/yum/\nenabled=1\ngpgcheck=1\ngpgkey=https://repo.charm.sh/yum/gpg.key\n' | sudo tee /etc/yum.repos.d/charm.repo >/dev/null
  sudo dnf install -y crush
fi
echo "[deploy] crush: $(command -v crush)"
echo "[deploy] launching crush (Olla provider preconfigured)…"
crush
`

// agentCatalog is the set of agents offered in the Agents section.
//
//   - Crush runs on the Olla server and talks to the Olla OpenAI endpoint (so it
//     load-balances across the whole worker pool). Installed from the Charm repo;
//     its provider config is written to ~/.config/crush/crush.json.
//   - OpenClaw and Hermes are deployed "by ollama" (ollama launch …) on a chosen
//     worker, after git + Node + a user npm prefix are bootstrapped.
//   - Nanoclaw runs as one or more Docker containers on a chosen worker (see
//     nanoclaw.go), so multiple isolated instances can share the same VM.
var agentCatalog = []agentDef{
	{
		name:       "Crush",
		cli:        "crush",
		deployable: true,
		target:     "gateway",
		endpoint:   "Olla OpenAI endpoint (whole pool)",
		desc:       "Charm coding agent",
	},
	{
		name:       "OpenClaw",
		cli:        "openclaw",
		deployable: true,
		target:     "worker",
		endpoint:   "local Ollama 127.0.0.1:11434",
		desc:       "Personal AI assistant",
	},
	{
		name:       "Hermes",
		cli:        "hermes",
		deployable: true,
		target:     "worker",
		endpoint:   "Olla OpenAI endpoint (whole pool)",
		desc:       "Nous Research self-improving agent",
	},
	{
		name:       "Nanoclaw",
		cli:        "nanoclaw",
		deployable: true,
		container:  true,
		target:     "worker",
		endpoint:   "Olla OpenAI endpoint (whole pool)",
		desc:       "Lightweight agent in Docker · multi-instance",
	},
	{
		name:       "OmniRoute",
		cli:        "omniroute",
		deployable: true,
		target:     "worker",
		endpoint:   "OpenAI-compatible :" + omniroutePort + " · 200+ providers",
		desc:       "AI gateway/router service in Docker",
	},
}

// agentDeployScript returns the install bootstrap for an agent. Crush uses the
// Charm repo; worker agents bootstrap deps then do a headless ollama launch
// (--yes --model is required for non-interactive installs). Hermes is then
// repointed at the Olla OpenAI endpoint so it uses the whole pool, and - when a
// Telegram token is configured - the messaging gateway is installed and started
// fully unattended (so deploy needs near-zero interaction).
func (m *model) agentDeployScript(a agentDef) string {
	if a.name == "Crush" {
		return crushDeployScript
	}
	if a.name == "Nanoclaw" {
		// Containerized: N isolated Docker containers on the chosen worker.
		return m.nanoclawDeployScript(maxInt(m.agentInstances, 1))
	}
	if a.name == "OmniRoute" {
		// Single long-running service container (the OmniRoute gateway).
		return m.omnirouteDeployScript()
	}
	model := m.effDefaultModel()
	gateway := a.name == "Hermes" && m.hermesGatewayWanted()

	var b strings.Builder
	b.WriteString("set -e\n")
	if gateway {
		// Make every hermes prompt_yes_no fall back to its default (yes).
		b.WriteString("export HERMES_NONINTERACTIVE=1\n")
	}
	b.WriteString(depsBootstrap)
	b.WriteString(fmt.Sprintf("if ! command -v %s >/dev/null 2>&1; then\n", a.cli))
	b.WriteString(fmt.Sprintf("  echo \"[deploy] installing %s (headless, model %s)…\"\n", a.cli, model))
	b.WriteString(fmt.Sprintf("  ollama launch %s --yes --model %s </dev/null || true\n", a.cli, model))
	b.WriteString("fi\n")
	if a.name == "Hermes" {
		b.WriteString(m.hermesOllaConfigScript(model))
	}
	if gateway {
		b.WriteString(m.hermesGatewayScript())
		b.WriteString("echo \"[deploy] Hermes gateway is ready (Telegram). You can close this window.\"\n")
	} else {
		b.WriteString(fmt.Sprintf("echo \"[deploy] launching %s…\"\n", a.cli))
		b.WriteString(a.cli + "\n")
	}
	return b.String()
}

// hermesGatewayWanted reports whether a Hermes deploy should also set up the
// messaging gateway (needs the feature enabled and a bot token configured).
func (m *model) hermesGatewayWanted() bool {
	return m.hermesCfg.GatewayEnabled && strings.TrimSpace(m.hermesCfg.TelegramBotToken) != ""
}

// hermesEnvPy upserts the Telegram keys in ~/.hermes/.env, replacing any
// existing (commented or live) lines and writing the file 0600. Only non-empty
// values are written. Inputs arrive via env vars so secrets stay out of code.
const hermesEnvPy = `import os, pathlib, re
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
print("[deploy] .env updated:", ",".join(keys) or "(none)")
`

// hermesGatewayScript writes the Telegram credentials to ~/.hermes/.env and
// installs + starts the messaging gateway unattended (user-level by default, or
// a root system service). It ends by printing gateway status for verification.
func (m *model) hermesGatewayScript() string {
	h := m.hermesCfg
	b64 := base64.StdEncoding.EncodeToString([]byte(hermesEnvPy))
	var b strings.Builder
	b.WriteString(`echo "[deploy] configuring Telegram credentials…"
PY="$HOME/.hermes/hermes-agent/venv/bin/python"; [ -x "$PY" ] || PY=python3
echo ` + b64 + ` | base64 -d > /tmp/oilsand-hermes-env.py
`)
	b.WriteString(fmt.Sprintf("TG_TOKEN='%s' TG_ALLOWED='%s' TG_HOME='%s' \"$PY\" /tmp/oilsand-hermes-env.py\n",
		shSingle(h.TelegramBotToken), shSingle(h.TelegramAllowedUsers), shSingle(h.TelegramHomeChannel)))
	b.WriteString("HERMES_BIN=\"$(command -v hermes || echo \"$HOME/.local/bin/hermes\")\"\n")
	if h.gatewayMode() == "system" {
		b.WriteString(`echo "[deploy] installing gateway (system service)…"
sudo HERMES_NONINTERACTIVE=1 HOME="$HOME" "$HERMES_BIN" gateway install --system --run-as-user "$USER" </dev/null
`)
	} else {
		b.WriteString(`echo "[deploy] installing gateway (user service)…"
HERMES_NONINTERACTIVE=1 "$HERMES_BIN" gateway install </dev/null
loginctl enable-linger "$USER" >/dev/null 2>&1 || true
HERMES_NONINTERACTIVE=1 "$HERMES_BIN" gateway start </dev/null || true
`)
	}
	b.WriteString(`echo "[deploy] gateway status:"
HERMES_NONINTERACTIVE=1 "$HERMES_BIN" gateway status </dev/null 2>&1 | head -20 || true
`)
	return b.String()
}

// hermesOllaPy rewrites ~/.hermes/config.yaml's model block to use the Olla
// OpenAI endpoint as a custom provider, preserving every other key. Run with
// Hermes' own venv python (which ships PyYAML). Inputs come from env vars so no
// shell quoting of user values is needed.
const hermesOllaPy = `import os, pathlib, yaml
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
print("[deploy] hermes -> Olla", m["base_url"], m["default"])
`

// hermesOllaConfigScript produces the shell that points Hermes at the Olla
// OpenAI endpoint. Hermes trusts a remote base_url when provider == "custom".
func (m *model) hermesOllaConfigScript(model string) string {
	base := strings.TrimRight(m.gateway, "/") + "/olla/openai/v1"
	key := orDefault(m.token, "olla")
	b64 := base64.StdEncoding.EncodeToString([]byte(hermesOllaPy))
	return fmt.Sprintf(`echo "[deploy] pointing hermes at Olla (%s)…"
PY="$HOME/.hermes/hermes-agent/venv/bin/python"; [ -x "$PY" ] || PY=python3
echo %s | base64 -d > /tmp/oilsand-hermes-olla.py
OLLA_BASE='%s' OLLA_KEY='%s' OLLA_MODEL='%s' "$PY" /tmp/oilsand-hermes-olla.py
`, base, b64, base, key, model)
}

// agentUninstallScript returns the shell that removes an agent install from a
// host. Best-effort by design: each step tolerates a partial/older install, and
// user data that isn't clearly the agent's own install tree is left alone
// (Crush keeps ~/.config/crush so a later reinstall finds its providers).
func agentUninstallScript(a agentDef) string {
	switch a.name {
	case "Crush":
		return `echo "[remove] uninstalling crush…"
sudo dnf -y remove crush 2>/dev/null || true
export PATH="$HOME/.npm-global/bin:$PATH"
npm uninstall -g crush >/dev/null 2>&1 || true
rm -f "$HOME/.local/bin/crush"
echo "[remove] crush removed (config kept in ~/.config/crush)"
`
	case "OpenClaw":
		return `echo "[remove] uninstalling openclaw…"
export PATH="$HOME/.npm-global/bin:$PATH"
npm uninstall -g openclaw >/dev/null 2>&1 || true
rm -f "$HOME/.local/bin/openclaw" "$HOME/.npm-global/bin/openclaw"
rm -rf "$HOME/.openclaw"
echo "[remove] openclaw removed"
`
	case "Hermes":
		return `echo "[remove] uninstalling hermes…"
HERMES_BIN="$(command -v hermes || echo "$HOME/.local/bin/hermes")"
if [ -x "$HERMES_BIN" ]; then
  HERMES_NONINTERACTIVE=1 "$HERMES_BIN" gateway stop </dev/null >/dev/null 2>&1 || true
  HERMES_NONINTERACTIVE=1 "$HERMES_BIN" gateway uninstall </dev/null >/dev/null 2>&1 || true
fi
systemctl --user disable --now hermes-gateway >/dev/null 2>&1 || true
sudo systemctl disable --now hermes-gateway >/dev/null 2>&1 || true
export PATH="$HOME/.npm-global/bin:$PATH"
npm uninstall -g hermes >/dev/null 2>&1 || true
rm -f "$HOME/.local/bin/hermes" "$HOME/.npm-global/bin/hermes"
rm -rf "$HOME/.hermes"
echo "[remove] hermes removed"
`
	case "OmniRoute":
		// Remove the service container; keep the omniroute-data volume so a later
		// redeploy finds its providers/keys (like Crush keeps ~/.config/crush).
		return `echo "[remove] stopping omniroute…"
sudo docker rm -f omniroute >/dev/null 2>&1 || true
echo "[remove] omniroute removed (config kept in the omniroute-data volume)"
`
	}
	return ""
}

// agentOpenCmd is the remote/login command "open" launches for an agent. Most
// agents just relaunch their CLI; OmniRoute is a background service, so opening
// it prints its dashboard URL and follows the container logs instead.
func agentOpenCmd(a agentDef) string {
	if a.name == "OmniRoute" {
		return loginShell(omnirouteConsoleScript())
	}
	return loginShell(a.cli)
}

// agentRemovedMsg reports the outcome of removing an agent's deployment(s):
// per-host uninstalls for host agents, or container deletions for Nanoclaw.
type agentRemovedMsg struct {
	agent     string
	container bool
	removed   int      // uninstalled hosts, or deleted containers
	okHosts   []string // hosts whose uninstall succeeded (host agents)
	errs      []string
}

// agentUninstallCmd runs an agent's uninstall script on every listed host in
// parallel (locally when a host is this machine) and reports which succeeded.
func agentUninstallCmd(a agentDef, hosts []string, user, pass string) tea.Cmd {
	script := agentUninstallScript(a)
	if script == "" {
		return func() tea.Msg { return notifyMsg(a.name + " has no uninstall routine") }
	}
	return func() tea.Msg {
		msg := agentRemovedMsg{agent: a.name}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, h := range hosts {
			wg.Add(1)
			go func(h string) {
				defer wg.Done()
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
				msg.okHosts = append(msg.okHosts, h)
			}(h)
		}
		wg.Wait()
		sort.Strings(msg.okHosts)
		sort.Strings(msg.errs)
		msg.removed = len(msg.okHosts)
		return msg
	}
}

func agentByName(name string) (agentDef, bool) {
	for _, a := range agentCatalog {
		if a.name == name {
			return a, true
		}
	}
	return agentDef{}, false
}

// agentHost resolves a default host for an agent: the gateway (Olla server) for
// gateway-targeted agents, else the first known Ollama worker.
func (m *model) agentHost(a agentDef) string {
	if a.target == "worker" {
		if ws := workersFromEndpoints(m.endpoints); len(ws) > 0 {
			return ws[0].host
		}
		return ""
	}
	return hostFromURL(m.gateway)
}

// loginShell wraps a simple command so it runs in a login shell, picking up the
// user's full PATH (ollama in /usr/local/bin, crush/npm bins in ~/.local/bin,
// etc.). Without this, `ssh host crush` runs in a minimal non-login PATH and
// fails with 127 even when the tool is installed.
func loginShell(cmd string) string {
	return "bash -lc '" + cmd + "'"
}

const crushConfigPath = "~/.config/crush/crush.json"

// crushConfigJSON builds a crush.json that registers the Olla gateway as an
// OpenAI-type provider named "olla" (so model names show through), pointing at
// the OpenAI-compatible base URL with the configured token and discovered
// models.
func (m *model) crushConfigJSON() string {
	base := strings.TrimRight(m.gateway, "/") + "/olla/openai/v1"
	key := orDefault(m.token, "olla")

	type cmodel struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		ContextWindow    int    `json:"context_window"`
		DefaultMaxTokens int    `json:"default_max_tokens"`
	}
	var models []cmodel
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		models = append(models, cmodel{ID: name, Name: name, ContextWindow: 32000, DefaultMaxTokens: 4096})
	}
	add(m.effDefaultModel())
	for _, md := range m.models {
		add(md.Name)
	}

	cfg := map[string]any{
		"$schema": "https://charm.land/crush.json",
		"providers": map[string]any{
			"olla": map[string]any{
				"type":     "openai",
				"base_url": base,
				"api_key":  key,
				"models":   models,
			},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b)
}
