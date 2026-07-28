package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

// installerBaseURL is the raw GitHub location the one-line installers live at.
const installerBaseURL = "https://raw.githubusercontent.com/script-repo/oilsand-ai-gateway/main/scripts"

// updateAction identifies one of the maintenance actions in the Update section.
type updateAction int

const (
	uaLocal updateAction = iota
	uaGateway
	uaAgents
	uaImage
	uaOS
	uaAll
	uaOllaKeys
)

// updateItem is a row in the Update list.
type updateItem struct {
	title  string
	desc   string
	action updateAction
}

func (i updateItem) Title() string       { return i.title }
func (i updateItem) Description() string { return i.desc }
func (i updateItem) FilterValue() string { return i.title }

// refreshUpdateList populates the Update section's list with the seven actions.
func (m *model) refreshUpdateList() {
	items := []list.Item{
		updateItem{"Update local machine", "download + run the latest installer for this OS", uaLocal},
		updateItem{"Update gateway (Olla)", "ssh to the gateway and reinstall the latest Olla", uaGateway},
		updateItem{"Update agents", "upgrade Crush, OpenClaw, Hermes, Nanoclaw, and OmniRoute containers", uaAgents},
		updateItem{"Update image", "change the image used for new deployments", uaImage},
		updateItem{"Update OS", "pick hosts, then run OS package updates over ssh", uaOS},
		updateItem{"Update all", "OS + agents + Olla + Ollama on every managed host", uaAll},
		updateItem{"Update Ollama cloud keys", "set OLLAMA_API_KEY on one or all workers", uaOllaKeys},
	}
	m.updateList.SetItems(items)
}

// updHost is a managed host with its role, used to build update plans.
type updHost struct {
	role string
	host string
}

// updateHosts collects the gateway, known workers, and any managed VMs with an
// IP, de-duplicated by host, as candidate targets for OS / Ollama updates.
func (m *model) updateHosts() []updHost {
	seen := map[string]bool{}
	var out []updHost
	add := func(role, h string) {
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, updHost{role, h})
	}
	if gw := hostFromURL(m.gateway); gw != "" {
		add("gateway", gw)
	}
	for _, w := range workersFromEndpoints(m.endpoints) {
		add("worker", w.host)
	}
	for _, v := range m.vms {
		if (v.Role == "gateway" || v.Role == "worker") && v.IP != "" {
			add(v.Role, v.IP)
		}
	}
	return out
}

func (m *model) firstWorkerHost() string {
	if ws := workersFromEndpoints(m.endpoints); len(ws) > 0 {
		return ws[0].host
	}
	return ""
}

// runSelectedUpdate dispatches the highlighted Update action: simple actions
// build a plan and stream it immediately; the rest open a modal first.
func (m *model) runSelectedUpdate() tea.Cmd {
	it, ok := m.updateList.SelectedItem().(updateItem)
	if !ok {
		return nil
	}
	switch it.action {
	case uaLocal:
		return m.startUpdatePlan([]updateStep{{
			title: "Update local machine", local: true, script: localInstallerScript(),
		}}, "update local machine")
	case uaGateway:
		return m.updateGatewayPlan()
	case uaAgents:
		return m.updateAgentsPlan()
	case uaImage:
		return m.openUpdateImage()
	case uaOS:
		return m.openOSUpdate()
	case uaAll:
		return m.openUpdateAllConfirm()
	case uaOllaKeys:
		return m.openOllaKey()
	}
	return nil
}

// startUpdatePlan streams a list of steps into the Output pane, reusing the
// proc-stream pipeline (procCh / handleProc) the Nutanix section uses.
func (m *model) startUpdatePlan(steps []updateStep, label string) tea.Cmd {
	if m.procBusy {
		m.notice = "a task is already running"
		return nil
	}
	if len(steps) == 0 {
		m.notice = "nothing to update"
		return nil
	}
	m.procBusy = true
	m.section = secUpdate
	m.zone = zoneContent
	m.logLines = append(m.logLines, fmt.Sprintf(">>> %s (%d step(s))", label, len(steps)))
	m.renderLog()
	m.procCh = make(chan ProcEvent, 256)
	go RunUpdatePlan(steps, m.procCh)
	return waitProc(m.procCh)
}

// localInstallerScript returns the one-line installer command for this OS.
// On Windows the script is invoked via powershell -File after download so a
// running oilsand-tui can self-update (install.ps1 renames the locked exe).
// Piping irm|iex still works from a plain shell; the Update section prefers
// a more reliable -File path when possible.
func localInstallerScript() string {
	if runtime.GOOS == "windows" {
		// Download then -File: clearer errors than irm|iex under -Command, and
		// avoids some ExecutionPolicy / pipeline quirks inside the TUI child.
		url := installerBaseURL + "/install.ps1"
		return "$ProgressPreference='SilentlyContinue'; " +
			"$p=Join-Path $env:TEMP ('oilsand-install-'+[guid]::NewGuid().ToString('n')+'.ps1'); " +
			"Invoke-WebRequest -UseBasicParsing -Uri '" + url + "' -OutFile $p; " +
			"try { & powershell -NoProfile -ExecutionPolicy Bypass -File $p; $c=$LASTEXITCODE } " +
			"finally { Remove-Item -Force $p -ErrorAction SilentlyContinue }; " +
			"if ($null -eq $c) { $c=0 }; exit $c"
	}
	return "curl -fsSL " + installerBaseURL + "/install.sh | sh"
}

// updateGatewayPlan reinstalls the latest Olla on the connected gateway.
func (m *model) updateGatewayPlan() tea.Cmd {
	gw := hostFromURL(m.gateway)
	if gw == "" {
		m.notice = "connect to a gateway first"
		return nil
	}
	script, err := os.ReadFile(localOllaScriptPath())
	if err != nil {
		m.notice = "could not read install-olla.sh: " + err.Error()
		return nil
	}
	step := updateStep{
		title: "Update Olla on gateway " + gw, host: gw,
		user: orDefault(m.sshUser, "rocky"), pass: m.sshPass,
		script: string(script), sudo: true,
	}
	return m.startUpdatePlan([]updateStep{step}, "update gateway (Olla)")
}

// updateAgentsPlan upgrades Crush (gateway, dnf) and re-launches OpenClaw and
// Hermes (workers, headless ollama launch); Hermes is re-pointed at Olla.
func (m *model) updateAgentsPlan() tea.Cmd {
	user := orDefault(m.sshUser, "rocky")
	model := m.effDefaultModel()
	var steps []updateStep

	if host := orDefault(m.agentReg["Crush"], hostFromURL(m.gateway)); host != "" {
		steps = append(steps, updateStep{
			title: "Update Crush on " + host, host: host, user: user, pass: m.sshPass,
			sudo:   true,
			script: "echo '[update] upgrading crush'\ndnf -y upgrade crush || dnf -y install crush",
		})
	}
	if host := orDefault(m.agentReg["OpenClaw"], m.firstWorkerHost()); host != "" {
		steps = append(steps, updateStep{
			title: "Update OpenClaw on " + host, host: host, user: user, pass: m.sshPass,
			script: fmt.Sprintf("echo '[update] re-launching openclaw'\nollama launch openclaw --yes --model %s </dev/null || true", model),
		})
	}
	if host := orDefault(m.agentReg["Hermes"], m.firstWorkerHost()); host != "" {
		script := "set -e\nexport HERMES_NONINTERACTIVE=1\n" + depsBootstrap +
			strings.ReplaceAll(hermesInstallFragment, "__HERMES_MODEL__", model) +
			m.hermesOllaConfigScript(model) +
			"echo '[update] Hermes updated and pointed at Olla'\n"
		steps = append(steps, updateStep{
			title: "Update Hermes on " + host, host: host, user: user, pass: m.sshPass, script: script,
		})
	}
	// Nanoclaw only updates where it was actually deployed (its containers live
	// on specific workers, unlike the ollama-launched agents) — one step per
	// worker that ever received a deploy.
	for _, host := range m.nanoclawHosts() {
		steps = append(steps, updateStep{
			title: "Update Nanoclaw containers on " + host, host: host, user: user, pass: m.sshPass,
			script: nanoclawUpdateScript(),
		})
	}
	for _, host := range m.omnirouteHosts() {
		steps = append(steps, updateStep{
			title: "Update OmniRoute on " + host, host: host, user: user, pass: m.sshPass,
			script: omnirouteUpdateScript(),
		})
	}
	if len(steps) == 0 {
		m.notice = "no agent hosts known — connect and refresh the pool first"
		return nil
	}
	return m.startUpdatePlan(steps, "update agents")
}

// osUpdateStep builds a privileged OS package-update step for one host.
func osUpdateStep(role, host, user, pass string) updateStep {
	return updateStep{
		title: "OS update on " + role + " " + host, host: host, user: user, pass: pass,
		sudo:   true,
		script: "echo '[update] dnf -y update'\ndnf -y update",
	}
}

// ollamaUpdateStep reinstalls the latest Ollama on a worker and restarts it.
func ollamaUpdateStep(host, user, pass string) updateStep {
	return updateStep{
		title: "Update Ollama on worker " + host, host: host, user: user, pass: pass,
		sudo:   true,
		script: "echo '[update] reinstalling ollama'\ncurl -fsSL https://ollama.com/install.sh | sh\nsystemctl restart ollama",
	}
}

// ollamaKeyStep writes OLLAMA_API_KEY into a dedicated systemd drop-in (so it
// doesn't clobber the deploy-time override.conf) and restarts ollama.
func ollamaKeyStep(host, user, pass, key string) updateStep {
	script := fmt.Sprintf(`echo '[update] applying OLLAMA_API_KEY'
mkdir -p /etc/systemd/system/ollama.service.d
printf '[Service]\nEnvironment="OLLAMA_API_KEY=%s"\n' '%s' > /etc/systemd/system/ollama.service.d/oilsand-key.conf
systemctl daemon-reload
systemctl restart ollama`, shSingle(key), shSingle(key))
	return updateStep{
		title: "Set Ollama key on " + host, host: host, user: user, pass: pass, sudo: true, script: script,
	}
}

// updateAllPlan builds the combined maintenance plan: OS on every host, agents,
// Olla on gateways, and Ollama on workers.
func (m *model) updateAllPlan() []updateStep {
	user := orDefault(m.sshUser, "rocky")
	hosts := m.updateHosts()
	var steps []updateStep
	for _, h := range hosts {
		steps = append(steps, osUpdateStep(h.role, h.host, user, m.sshPass))
	}
	if gw := hostFromURL(m.gateway); gw != "" {
		if script, err := os.ReadFile(localOllaScriptPath()); err == nil {
			steps = append(steps, updateStep{
				title: "Update Olla on gateway " + gw, host: gw, user: user, pass: m.sshPass,
				script: string(script), sudo: true,
			})
		}
	}
	for _, h := range hosts {
		if h.role == "worker" {
			steps = append(steps, ollamaUpdateStep(h.host, user, m.sshPass))
		}
	}
	if agents := m.updateAgentsSteps(); len(agents) > 0 {
		steps = append(steps, agents...)
	}
	return steps
}

// updateAgentsSteps returns the agent-update steps without starting them (so
// "Update all" can fold them into one plan).
func (m *model) updateAgentsSteps() []updateStep {
	user := orDefault(m.sshUser, "rocky")
	model := m.effDefaultModel()
	var steps []updateStep
	if host := orDefault(m.agentReg["Crush"], hostFromURL(m.gateway)); host != "" {
		steps = append(steps, updateStep{
			title: "Update Crush on " + host, host: host, user: user, pass: m.sshPass, sudo: true,
			script: "echo '[update] upgrading crush'\ndnf -y upgrade crush || dnf -y install crush",
		})
	}
	if host := orDefault(m.agentReg["OpenClaw"], m.firstWorkerHost()); host != "" {
		steps = append(steps, updateStep{
			title: "Update OpenClaw on " + host, host: host, user: user, pass: m.sshPass,
			script: fmt.Sprintf("ollama launch openclaw --yes --model %s </dev/null || true", model),
		})
	}
	if host := orDefault(m.agentReg["Hermes"], m.firstWorkerHost()); host != "" {
		script := "set -e\nexport HERMES_NONINTERACTIVE=1\n" + depsBootstrap +
			strings.ReplaceAll(hermesInstallFragment, "__HERMES_MODEL__", model) +
			m.hermesOllaConfigScript(model)
		steps = append(steps, updateStep{
			title: "Update Hermes on " + host, host: host, user: user, pass: m.sshPass, script: script,
		})
	}
	for _, host := range m.nanoclawHosts() {
		steps = append(steps, updateStep{
			title: "Update Nanoclaw containers on " + host, host: host, user: user, pass: m.sshPass,
			script: nanoclawUpdateScript(),
		})
	}
	for _, host := range m.omnirouteHosts() {
		steps = append(steps, updateStep{
			title: "Update OmniRoute on " + host, host: host, user: user, pass: m.sshPass,
			script: omnirouteUpdateScript(),
		})
	}
	return steps
}

// nanoclawHosts returns every worker Nanoclaw was deployed on, falling back to
// the single most-recent registration for configs saved before host lists.
func (m *model) nanoclawHosts() []string {
	if hs := m.agentHosts["Nanoclaw"]; len(hs) > 0 {
		return hs
	}
	if h := m.agentReg["Nanoclaw"]; h != "" {
		return []string{h}
	}
	return nil
}

// updateSummary is a short status string for the Output header.
func (m *model) updateSummary() string {
	return strconv.Itoa(len(m.updateHosts())) + " managed host(s)"
}
