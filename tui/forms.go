package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

func formWidth(m *model) int {
	w := m.contentW - 2
	if w <= 0 {
		w = 50
	}
	return clampInt(w, 38, 64)
}

func huhTheme() *huh.Theme { return huh.ThemeCharm() }

// huhKM is the form keymap with a reliable alternate for "previous field". Huh
// binds Prev to shift+tab only, but some terminals / VM & serial consoles never
// emit shift+tab, leaving the user unable to go back a field. ctrl+p is added as
// an alternate on every field type (and removed from Select/MultiSelect "up" so
// it means the same thing everywhere). Built once and shared by all forms.
var huhKM = newHuhKeyMap()

func newHuhKeyMap() *huh.KeyMap {
	k := huh.NewDefaultKeyMap()
	withPrev := func(b key.Binding) key.Binding {
		return key.NewBinding(
			key.WithKeys(append(b.Keys(), "ctrl+p")...),
			key.WithHelp("shift+tab/^p", "back"),
		)
	}
	k.Input.Prev = withPrev(k.Input.Prev)
	k.Text.Prev = withPrev(k.Text.Prev)
	k.Note.Prev = withPrev(k.Note.Prev)
	k.Confirm.Prev = withPrev(k.Confirm.Prev)
	k.Select.Prev = withPrev(k.Select.Prev)
	k.MultiSelect.Prev = withPrev(k.MultiSelect.Prev)
	// ctrl+p now means "previous field"; keep ↑/k (and ctrl+k) for moving within
	// a list so the two don't collide.
	k.Select.Up = key.NewBinding(key.WithKeys("up", "k", "ctrl+k"), key.WithHelp("↑", "up"))
	k.MultiSelect.Up = key.NewBinding(key.WithKeys("up", "k", "ctrl+k"), key.WithHelp("↑", "up"))
	return k
}

// selectOrInput returns a Huh select bound to val when opts is non-empty — so
// the user picks from the live Prism Central inventory — otherwise a free-text
// input, so the form still works when PC is unreachable or hasn't been polled.
// key lets onFormComplete read the chosen value via the form (copy-immune).
func selectOrInput(key, title, placeholder string, opts []string, val *string) huh.Field {
	if len(opts) == 0 {
		return huh.NewInput().Key(key).Title(title).Placeholder(placeholder).Value(val)
	}
	options := make([]huh.Option[string], 0, len(opts)+1)
	switch {
	case *val == "":
		*val = opts[0] // default to the first available rather than a baked-in name
	case !containsStr(opts, *val):
		// Keep a previously-saved value selectable even if PC no longer lists it.
		options = append(options, huh.NewOption(*val+" (saved)", *val))
	}
	for _, o := range opts {
		options = append(options, huh.NewOption(o, o))
	}
	return huh.NewSelect[string]().Key(key).Title(title).Options(options...).Value(val)
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// effDefaultModel is the model new workers/chat should use: the persisted
// pool-wide default if set, otherwise the built-in default.
func (m *model) effDefaultModel() string { return orDefault(m.defModel, DefaultModel) }

// openConnect builds the gateway-connection modal.
func (m *model) openConnect() tea.Cmd {
	m.fGateway = m.gateway
	m.fSSHUser = orDefault(m.sshUser, "rocky")
	m.fSSHPass = m.sshPass
	m.modal = modalConnect
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Key("gateway").Title("Olla gateway URL").
			Placeholder("http://gateway-host:40114").Value(&m.fGateway),
		huh.NewInput().Key("sshuser").Title("SSH user").Description("used to edit endpoints on the gateway VM").
			Placeholder("rocky").Value(&m.fSSHUser),
		huh.NewInput().Key("sshpass").Title("SSH password").Password(true).Value(&m.fSSHPass),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openEndpoint builds the "add Ollama endpoint" modal.
func (m *model) openEndpoint() tea.Cmd {
	if m.gateway == "" {
		m.notice = "connect to a gateway first"
		return nil
	}
	m.fEpName, m.fEpURL, m.fEpType, m.fEpPrio = "", "", "ollama", "100"
	m.modal = modalEndpoint
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Key("epname").Title("Endpoint name").Placeholder("ollama-worker-03").Value(&m.fEpName),
		huh.NewInput().Key("epurl").Title("Endpoint URL").Placeholder("http://worker-host:11434").Value(&m.fEpURL),
		huh.NewSelect[string]().Key("eptype").Title("Type").
			Options(huh.NewOptions("ollama", "openai", "vllm", "lmstudio")...).Value(&m.fEpType),
		huh.NewInput().Key("epprio").Title("Priority").Placeholder("100").Value(&m.fEpPrio),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openPull builds the "pull model" modal.
func (m *model) openPull() tea.Cmd {
	if m.client == nil {
		m.notice = "connect to a gateway first"
		return nil
	}
	m.fModel = ""
	m.modal = modalPull
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Key("model").Title("Model to pull").
			Description("downloads through the gateway's Ollama proxy").
			Placeholder("llama3.2:3b").Value(&m.fModel),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openCatalog builds the "browse & download models" modal from the curated
// catalog (including Ollama Cloud models). It's a multi-select so several models
// can be queued at once; every pick is pulled to every worker. The selection is
// read back via the form's keyed result (m.form.Get) rather than a bound pointer
// so it's immune to Bubble Tea's per-update model copying.
func (m *model) openCatalog() tea.Cmd {
	if len(workersFromEndpoints(m.endpoints)) == 0 {
		m.notice = "no workers known yet — open Pool and press r first"
		return nil
	}
	m.modal = modalCatalog
	opts := make([]huh.Option[string], 0, len(modelCatalog))
	for _, c := range modelCatalog {
		opts = append(opts, huh.NewOption(c.Title()+"  —  "+c.Description(), c.name))
	}
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Key("models").
			Title("Download models to all workers").
			Description("space toggles · enter confirms · ☁ = Ollama Cloud (needs `ollama signin`)").
			Options(opts...),
	)).WithWidth(formWidth(m)).WithHeight(18).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openDeploy builds the Nutanix deploy modal pre-selected to role.
func (m *model) openDeploy(role string) tea.Cmd {
	if m.pcCfg == nil {
		m.notice = "Prism Central not configured"
		return nil
	}
	if m.procBusy {
		m.notice = "a deploy/delete is already running"
		return nil
	}
	dcfg := withDeployDefaults(m.deployCfg)
	var missing []string
	if dcfg.ImageName == "" {
		missing = append(missing, "image")
	}
	if dcfg.ClusterName == "" {
		missing = append(missing, "cluster")
	}
	if dcfg.SubnetName == "" {
		missing = append(missing, "subnet")
	}
	if dcfg.VMPassword == "" {
		missing = append(missing, "VM password")
	}
	if len(missing) > 0 {
		m.notice = "set " + strings.Join(missing, ", ") + " in Nutanix settings first"
		return m.openNutanixCfg()
	}
	def := m.effDefaultModel()
	m.fRole, m.fName, m.fModel, m.fCount = role, "", def, "1"
	m.modal = modalDeploy
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Key("role").Title("Role").
			Options(huh.NewOptions("worker", "gateway")...).Value(&m.fRole),
		huh.NewInput().Key("vmname").Title("VM name").Description("blank = auto-increment (ollama-worker-NN)").Value(&m.fName),
		huh.NewInput().Key("count").Title("Instances").
			Description("worker only · number of workers to deploy in parallel").Value(&m.fCount),
		huh.NewInput().Key("model").Title("Model").Description("worker only · defaults to the current default model").
			Placeholder(def).Value(&m.fModel),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openNutanixCfg builds the Nutanix settings modal: Prism Central instance +
// service account, the VM template parameters, and the image/cluster/subnet to
// clone from. Values persist to tui.json and apply to subsequent deploys.
func (m *model) openNutanixCfg() tea.Cmd {
	d := withDeployDefaults(m.deployCfg)
	// Prefer the persisted override; fall back to the live PC config so the
	// fields show what's actually in use.
	m.fPCHost, m.fPCPort, m.fPCKey = m.pcOver.Host, m.pcOver.Port, m.pcOver.APIKey
	m.fPCUser, m.fPCPass = m.pcOver.User, m.pcOver.Password
	if m.fPCHost == "" && m.pcCfg != nil {
		m.fPCHost, m.fPCPort = m.pcCfg.Host, m.pcCfg.Port
	}
	m.fPCPort = orDefault(m.fPCPort, "9440")
	m.fImage, m.fCluster, m.fSubnet = d.ImageName, d.ClusterName, d.SubnetName
	m.fSockets = strconv.Itoa(d.NumSockets)
	m.fCores = strconv.Itoa(d.CoresPerSocket)
	m.fMem = strconv.Itoa(d.MemoryGiB)
	m.fDisk = strconv.Itoa(d.DiskGiB)
	m.fVMUser, m.fVMPass = d.VMUser, d.VMPassword

	m.modal = modalNutanixCfg
	m.form = huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Title("Prism Central").
				Description("Leave the API key blank to use a user/password service account instead."),
			huh.NewInput().Key("pchost").Title("PC host / IP").Placeholder("prism-central host or IP").Value(&m.fPCHost),
			huh.NewInput().Key("pcport").Title("PC port").Placeholder("9440").Value(&m.fPCPort),
			huh.NewInput().Key("pckey").Title("API key").Password(true).
				Description("X-Ntnx-Api-Key (preferred)").Value(&m.fPCKey),
			huh.NewInput().Key("pcuser").Title("Service account user").Placeholder("admin").Value(&m.fPCUser),
			huh.NewInput().Key("pcpass").Title("Service account password").Password(true).Value(&m.fPCPass),
		),
		huh.NewGroup(
			huh.NewNote().Title("VM template").Description("Resources for newly deployed gateways/workers."),
			huh.NewInput().Key("sockets").Title("Sockets").Placeholder("2").Value(&m.fSockets),
			huh.NewInput().Key("cores").Title("Cores / socket").Placeholder("4").Value(&m.fCores),
			huh.NewInput().Key("mem").Title("Memory (GiB)").Placeholder("12").Value(&m.fMem),
			huh.NewInput().Key("disk").Title("Disk (GiB)").Placeholder("50").Value(&m.fDisk),
			huh.NewInput().Key("vmuser").Title("VM user").Placeholder("rocky").Value(&m.fVMUser),
			huh.NewInput().Key("vmpass").Title("VM password").Password(true).Value(&m.fVMPass),
		),
		huh.NewGroup(
			huh.NewNote().Title("Image & placement").
				Description(fmt.Sprintf("Live from Prism Central: %d images, %d clusters, %d subnets. "+
					"If a count is 0, PC wasn't reachable — set the PC fields above, save, then reopen to get dropdowns.",
					len(m.images), len(m.clusters), len(m.subnets))),
			selectOrInput("image", "Image name", "disk image to clone", m.images, &m.fImage),
			selectOrInput("cluster", "Cluster", "target cluster", m.clusters, &m.fCluster),
			selectOrInput("subnet", "Subnet", "target subnet", m.subnets, &m.fSubnet),
		),
	).WithWidth(formWidth(m)).WithHeight(18).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	cmd := m.form.Init()
	// Refresh PC inventory in the background when something's missing so the
	// counts/notice update and the next open offers dropdowns.
	if m.pcCfg != nil && (len(m.clusters) == 0 || len(m.images) == 0 || len(m.subnets) == 0) {
		return tea.Batch(cmd, vmsCmd(m.pcCfg))
	}
	return cmd
}

// openHermesCfg builds the Hermes gateway / Telegram settings modal. These
// one-time inputs (bot token + allowed user IDs) let subsequent Hermes deploys
// install and start the messaging gateway with near-zero interaction.
func (m *model) openHermesCfg() tea.Cmd {
	h := m.hermesCfg
	m.fTgToken = h.TelegramBotToken
	m.fTgAllowed = h.TelegramAllowedUsers
	m.fTgHome = h.TelegramHomeChannel
	m.fGwMode = h.gatewayMode()
	// Default the toggle on for a fresh config (no token yet means first run).
	m.fGwEnable = h.GatewayEnabled || h.TelegramBotToken == ""

	m.modal = modalHermesCfg
	m.form = huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Title("Hermes Telegram gateway").
				Description("Create a bot with @BotFather, paste its token here. Saved to tui.json and reused for every Hermes deploy."),
			huh.NewInput().Key("tgtoken").Title("Bot token").Password(true).
				Placeholder("123456789:AA...").Value(&m.fTgToken),
			huh.NewInput().Key("tgallowed").Title("Allowed Telegram user IDs").
				Description("comma-separated; blank = pair via DM after deploy").Value(&m.fTgAllowed),
			huh.NewInput().Key("tghome").Title("Home channel (optional)").
				Description("default chat ID for cron/notifications").Value(&m.fTgHome),
			huh.NewSelect[string]().Key("gwmode").Title("Gateway service").
				Options(
					huh.NewOption("user (no sudo, runs at login via linger)", "user"),
					huh.NewOption("system (root, starts at boot)", "system"),
				).Value(&m.fGwMode),
			huh.NewConfirm().Key("gwenable").Title("Auto-setup gateway on Hermes deploy?").
				Affirmative("Yes").Negative("No").Value(&m.fGwEnable),
		),
	).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openUpdateImage lets the operator change the image used for new deployments,
// from the live Prism Central image list (free-text fallback when PC is down).
// imagePreset is a ready-to-seed cloud image: a friendly label, the name it gets
// in Prism Central, and the source URL.
type imagePreset struct {
	label string
	name  string
	url   string
}

var imagePresets = []imagePreset{
	{"Rocky Linux 9 (GenericCloud)", "Rocky-9-GenericCloud", "https://dl.rockylinux.org/pub/rocky/9/images/x86_64/Rocky-9-GenericCloud-Base.latest.x86_64.qcow2"},
	{"Rocky Linux 10 (GenericCloud)", "Rocky-10-GenericCloud", "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2"},
	{"Ubuntu 24.04 (Noble cloud)", "Ubuntu-24.04-Noble", "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"},
}

func (m *model) openUpdateImage() tea.Cmd {
	m.fUpdImage = withDeployDefaults(m.deployCfg).ImageName
	m.fSeedName, m.fSeedURL, m.fSeedPreset = "", "", ""
	m.modal = modalUpdateImage
	presetOpts := []huh.Option[string]{huh.NewOption("— none / custom URL below —", "")}
	for _, p := range imagePresets {
		presetOpts = append(presetOpts, huh.NewOption(p.label, p.url))
	}
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Deployment image").
			Description("Pick an existing image, or seed a new one below. Applies to future deploys; saved to tui.json."),
		selectOrInput("image", "Image name", "disk image to clone", m.images, &m.fUpdImage),
		huh.NewNote().Title("Seed a new image (optional)").
			Description("Import a cloud image into Prism Central, then set it as the deployment image."),
		huh.NewSelect[string]().Key("preset").Title("Preset image").
			Options(presetOpts...).Value(&m.fSeedPreset),
		huh.NewInput().Key("seedname").Title("New image name").
			Description("blank = preset default or URL filename").
			Placeholder("Rocky-9-GenericCloud").Value(&m.fSeedName),
		huh.NewInput().Key("seedurl").Title("…or custom image URL (qcow2)").
			Description("overrides the preset when set").
			Placeholder("https://.../Rocky-9-GenericCloud-Base.latest.x86_64.qcow2").Value(&m.fSeedURL),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	cmd := m.form.Init()
	if m.pcCfg != nil && len(m.images) == 0 {
		return tea.Batch(cmd, vmsCmd(m.pcCfg))
	}
	return cmd
}

// imageNameFromURL derives a sensible image name from a URL's filename.
func imageNameFromURL(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	return u
}

// presetImageName returns the preset's clean name for a known URL, else the
// derived filename.
func presetImageName(url string) string {
	for _, p := range imagePresets {
		if p.url == url {
			return p.name
		}
	}
	return imageNameFromURL(url)
}

// startImageSeed imports a disk image into Prism Central from a URL, streaming
// the helper's output into the Update section's Output pane (it can take a while
// for large qcow2 images). The deployment image is set to the new name first.
func (m *model) startImageSeed(name, url string) tea.Cmd {
	if m.pcCfg == nil {
		m.notice = "Prism Central not configured"
		return nil
	}
	if m.procBusy {
		m.notice = "a task is already running"
		return nil
	}
	m.procBusy = true
	m.section = secUpdate
	m.zone = zoneContent
	args := []string{"seed-image", "--name", name, "--image-url", url}
	m.logLines = append(m.logLines, ">>> seed image "+name+": nutanix_olla_vm.py "+strings.Join(args, " "))
	m.renderLog()
	m.procCh = make(chan ProcEvent, 128)
	go RunVMScript(m.pcCfg, args, m.procCh)
	return waitProc(m.procCh)
}

// openOSUpdate lets the operator pick which managed hosts get OS package updates.
func (m *model) openOSUpdate() tea.Cmd {
	hosts := m.updateHosts()
	if len(hosts) == 0 {
		m.notice = "no managed hosts known — connect and refresh the pool first"
		return nil
	}
	opts := make([]huh.Option[string], 0, len(hosts))
	for _, h := range hosts {
		opts = append(opts, huh.NewOption(h.role+"  "+h.host, h.host))
	}
	m.fOSHosts = nil
	m.modal = modalOSUpdate
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Key("oshosts").
			Title("Update OS on which hosts?").
			Description("space to toggle · enter to run").
			Options(opts...).Value(&m.fOSHosts),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openOllaKey collects an OLLAMA_API_KEY and the worker(s) to apply it to.
func (m *model) openOllaKey() tea.Cmd {
	workers := workersFromEndpoints(m.endpoints)
	if len(workers) == 0 {
		m.notice = "no workers known — open Pool and press r first"
		return nil
	}
	m.fOllaKey = ""
	m.fOllaTarget = "all"
	opts := []huh.Option[string]{huh.NewOption("all workers", "all")}
	for _, w := range workers {
		opts = append(opts, huh.NewOption(w.name+"  ("+w.host+")", w.host))
	}
	m.modal = modalOllaKey
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Key("ollakey").Title("OLLAMA_API_KEY").Password(true).
			Description("written to a systemd drop-in; applied, not saved to tui.json").Value(&m.fOllaKey),
		huh.NewSelect[string]().Key("ollatarget").Title("Apply to").Options(opts...).Value(&m.fOllaTarget),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openUpdateAllConfirm asks for confirmation before the combined update plan.
func (m *model) openUpdateAllConfirm() tea.Cmd {
	m.fUpdConfirm = false
	m.modal = modalUpdateAll
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Key("updall").
			Title("Update everything?").
			Description("OS updates on all managed hosts, agents, Olla on gateways, and Ollama on workers.").
			Affirmative("Yes").Negative("No").Value(&m.fUpdConfirm),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openCustomDeploy collects a new custom deployment type: a friendly name and
// the URL of a setup script run on the VM (curl | sudo bash) after it boots.
func (m *model) openCustomDeploy() tea.Cmd {
	m.fCustName, m.fCustURL = "", ""
	m.fCustScheme, m.fCustPort, m.fCustPath = "http", "", ""
	m.modal = modalCustomDeploy
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Key("custname").Title("Deployment name").
			Placeholder("e.g. postgres-node").Value(&m.fCustName),
		huh.NewInput().Key("custurl").Title("Setup script").
			Description("a URL (downloaded + sudo bash'd) OR a full shell command run on the VM").
			Placeholder("https://example.com/setup.sh  or  curl -fsSL <url> | sudo bash").Value(&m.fCustURL),
		huh.NewNote().Title("Workload access link (optional)").
			Description("shown as a clickable link after deploy: <scheme>://<vm-ip>:<port><path>"),
		huh.NewSelect[string]().Key("custscheme").Title("Scheme").
			Options(huh.NewOptions("http", "https")...).Value(&m.fCustScheme),
		huh.NewInput().Key("custport").Title("Workload port").
			Placeholder("e.g. 8080").Value(&m.fCustPort),
		huh.NewInput().Key("custpath").Title("Path").
			Description("optional, e.g. /dashboard").Value(&m.fCustPath),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openAgentHostPick lets the user choose which worker an agent is opened/deployed
// on (item: "specify the worker that hermes or openclaw is deployed on").
func (m *model) openAgentHostPick(agentName, act string) tea.Cmd {
	ws := workersFromEndpoints(m.endpoints)
	if len(ws) == 0 {
		m.notice = "no workers known — open Pool and press r first"
		return nil
	}
	m.pendingAgent, m.pendingAct = agentName, act
	m.fAgentHost = ws[0].host
	if h, ok := m.agentReg[agentName]; ok && h != "" {
		m.fAgentHost = h
	}
	opts := make([]huh.Option[string], 0, len(ws))
	for _, w := range ws {
		opts = append(opts, huh.NewOption(w.name+"  ("+w.host+")", w.host))
	}
	verb := act
	if verb != "" {
		verb = strings.ToUpper(verb[:1]) + verb[1:]
	}
	fields := []huh.Field{
		huh.NewSelect[string]().Key("agenthost").
			Title(verb + " " + agentName + " on which worker?").
			Options(opts...).Value(&m.fAgentHost),
	}
	// Containerized agents (Nanoclaw) can run several isolated instances on the
	// same worker — ask how many containers to start.
	if a, ok := agentByName(agentName); ok && a.container && act == "deploy" {
		m.fAgentCount = "1"
		fields = append(fields, huh.NewInput().Key("agentcount").Title("Instances").
			Description("number of "+agentName+" containers to start on this worker (each isolated)").
			Placeholder("1").Value(&m.fAgentCount))
	}
	m.modal = modalAgentHost
	m.form = huh.NewForm(huh.NewGroup(fields...)).
		WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openAgentRemove builds the remove picker for a host-installed agent: which
// deployment (host) to uninstall, or all of them when it lives on several.
func (m *model) openAgentRemove(a agentDef, hosts []string) tea.Cmd {
	m.pendingAgent = a.name
	opts := make([]huh.Option[string], 0, len(hosts)+1)
	for _, h := range hosts {
		opts = append(opts, huh.NewOption(a.name+"  @ "+h, h))
	}
	if len(hosts) > 1 {
		opts = append(opts, huh.NewOption(fmt.Sprintf("every host (%d)", len(hosts)), "all"))
	}
	m.fRemoveTarget = hosts[0]
	m.modal = modalAgentRemove
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Remove "+a.name).
			Description("Uninstalls the agent from the host and clears its registration. Esc cancels."),
		huh.NewSelect[string]().Key("rmtarget").Title("Remove which install?").
			Options(opts...).Value(&m.fRemoveTarget),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// openNanoclawRemove builds the remove picker for Nanoclaw from the live
// container inventory (fetched just before this opens): a single instance on a
// specific worker, or every instance everywhere, plus whether to delete the
// per-instance state volume(s).
func (m *model) openNanoclawRemove() tea.Cmd {
	if len(m.nanoInst) == 0 {
		if len(m.nanoInstErrs) > 0 {
			m.notice = "no nanoclaw instances found; some hosts were unreachable — fix SSH access and retry"
		} else {
			m.notice = "no nanoclaw containers left on any worker"
		}
		return nil
	}
	opts := make([]huh.Option[string], 0, len(m.nanoInst)+1)
	for _, r := range m.nanoInst {
		opts = append(opts, huh.NewOption(
			fmt.Sprintf("%s  @ %s   (%s)", r.name, r.host, r.status),
			r.host+"|"+r.name))
	}
	if len(m.nanoInst) > 1 {
		opts = append(opts, huh.NewOption(fmt.Sprintf("all %d instances on every worker", len(m.nanoInst)), "all"))
	}
	m.pendingAgent = "Nanoclaw"
	m.fRemoveTarget = m.nanoInst[0].host + "|" + m.nanoInst[0].name
	m.fRemoveVols = true
	m.modal = modalAgentRemove
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Remove Nanoclaw instance(s)").
			Description("Deletes the selected container(s). Esc cancels."),
		huh.NewSelect[string]().Key("rmtarget").Title("Which instance?").
			Options(opts...).Value(&m.fRemoveTarget),
		huh.NewConfirm().Key("rmvols").Title("Also delete the state volume(s)?").
			Description("oilsand-<name> — No keeps the state for a future instance with the same name").
			Affirmative("Yes").Negative("No").Value(&m.fRemoveVols),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme()).WithKeyMap(huhKM)
	return m.form.Init()
}

// fstr reads a form field's value by key and trims it. Reading via the form
// (rather than the Value(&…)-bound model field) is immune to Bubble Tea's
// per-update model copying, which otherwise drops the user's typed edits.
func (m *model) fstr(key string) string {
	if m.form == nil {
		return ""
	}
	return strings.TrimSpace(m.form.GetString(key))
}

// fsecret is like fstr but preserves the value verbatim (no trim) for passwords.
func (m *model) fsecret(key string) string {
	if m.form == nil {
		return ""
	}
	return m.form.GetString(key)
}

// onFormComplete dispatches the action for the just-submitted modal.
func (m *model) onFormComplete() tea.Cmd {
	switch m.modal {
	case modalConnect:
		gw := normalizeGateway(m.fstr("gateway"))
		if gw == "" {
			m.notice = "enter a gateway URL"
			return nil
		}
		m.gateway = gw
		m.sshUser = orDefault(m.fstr("sshuser"), "rocky")
		m.sshPass = m.fsecret("sshpass")
		m.connInfo = "connecting…"
		// Remember the connection so later launches skip first-run setup.
		_ = saveConnect(m.tokFile, m.gateway, m.sshUser, m.sshPass)
		return connectCmd(gw)

	case modalEndpoint:
		name := m.fstr("epname")
		url := m.fstr("epurl")
		if name == "" || url == "" {
			m.notice = "endpoint name and URL are required"
			return nil
		}
		prio, _ := strconv.Atoi(m.fstr("epprio"))
		if prio == 0 {
			prio = 100
		}
		e := endpointEntry{
			Name: name, URL: url,
			Type:          orDefault(m.fstr("eptype"), "ollama"),
			Priority:      prio,
			CheckInterval: "10s", CheckTimeout: "3s",
		}
		host := hostFromURL(m.gateway)
		m.notice = "adding " + name + " via SSH to " + host + " …"
		return sshAddAndKeyCmd(host, orDefault(m.sshUser, "rocky"), m.sshPass, e)

	case modalPull:
		name := m.fstr("model")
		if name == "" {
			m.notice = "enter a model name to pull"
			return nil
		}
		return m.startPull(name)

	case modalCatalog:
		var names []string
		if m.form != nil {
			if v, ok := m.form.Get("models").([]string); ok {
				names = v
			}
		}
		if len(names) == 0 {
			m.notice = "no model selected"
			return nil
		}
		workers := workersFromEndpoints(m.endpoints)
		if len(workers) == 0 {
			m.notice = "no workers known — open Pool and press r"
			return nil
		}
		label := fmt.Sprintf("downloading %d model(s) to all workers…", len(names))
		return m.startMultiPull(names, workers, true, label)

	case modalDeploy:
		role := orDefault(m.fstr("role"), m.fRole)
		name := m.fstr("vmname")
		if role == "gateway" {
			args := []string{"pattern-a"}
			args = append(args, m.deployFlags()...)
			if name != "" {
				args = append(args, "--vm-name", name)
			}
			return m.startProc(args, "deploy gateway")
		}
		if m.gateway == "" {
			m.notice = "connect to the target Olla gateway first"
			return nil
		}
		model := orDefault(m.fstr("model"), m.effDefaultModel())
		count := atoiOr(m.fstr("count"), 1)
		if count > 1 {
			return m.startWorkerBatch(count, model, name)
		}
		args := []string{"pattern-b", "--model", model, "--olla-url", m.gateway}
		args = append(args, m.deployFlags()...)
		if name != "" {
			args = append(args, "--vm-name", name)
		}
		return m.startProc(args, "deploy worker")

	case modalNutanixCfg:
		d := withDeployDefaults(deploySettings{
			ImageName:      m.fstr("image"),
			ClusterName:    m.fstr("cluster"),
			SubnetName:     m.fstr("subnet"),
			NumSockets:     atoiOr(m.fstr("sockets"), 0),
			CoresPerSocket: atoiOr(m.fstr("cores"), 0),
			MemoryGiB:      atoiOr(m.fstr("mem"), 0),
			DiskGiB:        atoiOr(m.fstr("disk"), 0),
			VMUser:         m.fstr("vmuser"),
			VMPassword:     m.fsecret("vmpass"),
		})
		o := pcOverride{
			Host:     m.fstr("pchost"),
			Port:     m.fstr("pcport"),
			APIKey:   m.fstr("pckey"),
			User:     m.fstr("pcuser"),
			Password: m.fsecret("pcpass"),
		}
		m.deployCfg = d
		m.pcOver = o
		_ = saveDeployPC(m.tokFile, d, o)
		if pc := pcConfigFromOverride(o); pc != nil {
			m.pcCfg = pc
		} else {
			m.pcCfg = LoadPCConfig()
		}
		m.notice = "Nutanix settings saved"
		if m.pcCfg != nil {
			return vmsCmd(m.pcCfg)
		}
		return nil

	case modalAgentHost:
		a, ok := agentByName(m.pendingAgent)
		if !ok {
			return nil
		}
		m.agentInstances = atoiOr(m.fstr("agentcount"), 1)
		return m.startAgent(a, m.pendingAct, orDefault(m.fstr("agenthost"), m.fAgentHost))

	case modalAgentRemove:
		a, ok := agentByName(m.pendingAgent)
		if !ok {
			return nil
		}
		target := orDefault(m.fstr("rmtarget"), m.fRemoveTarget)
		user := orDefault(m.sshUser, "rocky")
		if a.container {
			vols := m.fRemoveVols
			if m.form != nil {
				vols = m.form.GetBool("rmvols")
			}
			var targets []nanoclawInstance
			if target == "all" {
				targets = append(targets, m.nanoInst...)
			} else if host, name, ok := strings.Cut(target, "|"); ok && host != "" && name != "" {
				targets = append(targets, nanoclawInstance{host: host, name: name})
			}
			if len(targets) == 0 {
				m.notice = "nothing selected to remove"
				return nil
			}
			m.notice = fmt.Sprintf("removing %d nanoclaw instance(s)…", len(targets))
			return nanoclawRemoveCmd(targets, vols, user, m.sshPass)
		}
		hosts := m.agentDeployedHosts(a.name)
		if target != "all" {
			hosts = []string{target}
		}
		if len(hosts) == 0 {
			m.notice = "nothing selected to remove"
			return nil
		}
		m.notice = fmt.Sprintf("uninstalling %s on %d host(s)…", a.name, len(hosts))
		return agentUninstallCmd(a, hosts, user, m.sshPass)

	case modalHermesCfg:
		enable := m.fGwEnable
		if m.form != nil {
			enable = m.form.GetBool("gwenable")
		}
		h := hermesSettings{
			TelegramBotToken:     m.fstr("tgtoken"),
			TelegramAllowedUsers: m.fstr("tgallowed"),
			TelegramHomeChannel:  m.fstr("tghome"),
			GatewayEnabled:       enable,
			GatewayMode:          orDefault(m.fstr("gwmode"), "user"),
		}
		m.hermesCfg = h
		_ = saveHermesCfg(m.tokFile, h)
		m.refreshAgents()
		m.notice = "Hermes gateway settings saved"
		return nil

	case modalUpdateImage:
		// If a source URL (custom field or preset) was given, seed (import) that
		// image into Prism Central and adopt it as the deployment image;
		// otherwise just switch to a pre-existing image.
		seedURL := m.fstr("seedurl")
		if seedURL == "" {
			seedURL = m.fstr("preset")
		}
		if seedURL != "" {
			seedName := m.fstr("seedname")
			if seedName == "" {
				seedName = presetImageName(seedURL)
			}
			if seedName == "" {
				m.notice = "enter a name for the new image"
				return nil
			}
			m.deployCfg.ImageName = seedName
			_ = saveDeployPC(m.tokFile, withDeployDefaults(m.deployCfg), m.pcOver)
			m.notice = "importing image " + seedName + " into Prism Central — see Output"
			return m.startImageSeed(seedName, seedURL)
		}
		img := m.fstr("image")
		if img == "" {
			m.notice = "pick an image"
			return nil
		}
		m.deployCfg.ImageName = img
		_ = saveDeployPC(m.tokFile, withDeployDefaults(m.deployCfg), m.pcOver)
		m.notice = "deployment image set to " + img
		return nil

	case modalOSUpdate:
		var hosts []string
		if m.form != nil {
			if v, ok := m.form.Get("oshosts").([]string); ok {
				hosts = v
			}
		}
		if len(hosts) == 0 {
			m.notice = "no hosts selected"
			return nil
		}
		roleByHost := map[string]string{}
		for _, h := range m.updateHosts() {
			roleByHost[h.host] = h.role
		}
		user := orDefault(m.sshUser, "rocky")
		var steps []updateStep
		for _, h := range hosts {
			steps = append(steps, osUpdateStep(orDefault(roleByHost[h], "host"), h, user, m.sshPass))
		}
		return m.startUpdatePlan(steps, "OS update")

	case modalOllaKey:
		key := m.fsecret("ollakey")
		if strings.TrimSpace(key) == "" {
			m.notice = "enter an OLLAMA_API_KEY"
			return nil
		}
		target := orDefault(m.fstr("ollatarget"), "all")
		user := orDefault(m.sshUser, "rocky")
		var steps []updateStep
		for _, w := range workersFromEndpoints(m.endpoints) {
			if target == "all" || target == w.host {
				steps = append(steps, ollamaKeyStep(w.host, user, m.sshPass, key))
			}
		}
		return m.startUpdatePlan(steps, "update Ollama cloud keys")

	case modalUpdateAll:
		confirm := m.fUpdConfirm
		if m.form != nil {
			confirm = m.form.GetBool("updall")
		}
		if !confirm {
			m.notice = "update all cancelled"
			return nil
		}
		return m.startUpdatePlan(m.updateAllPlan(), "update all")

	case modalCustomDeploy:
		name := m.fstr("custname")
		url := m.fstr("custurl")
		if name == "" || url == "" {
			m.notice = "deployment name and setup script (URL or command) are required"
			return nil
		}
		m.customDeploys = append(m.customDeploys, customDeploy{
			Name: name, ScriptURL: url,
			Scheme: m.fstr("custscheme"), Port: m.fstr("custport"), Path: m.fstr("custpath"),
		})
		_ = saveCustomDeploys(m.tokFile, m.customDeploys)
		m.refreshCustomList()
		// Highlight the just-added config (not the "add" row) so the next Enter
		// deploys it instead of re-opening this form.
		m.customList.Select(len(m.customList.Items()) - 1)
		m.nutanixCustom = true
		m.notice = "added custom deployment: " + name + " — press enter to deploy it"
		return nil
	}
	return nil
}

// deployFlags maps the persisted VM template + image settings onto the Python
// helper's CLI flags (shared across pattern-a/pattern-b).
func (m *model) deployFlags() []string {
	d := withDeployDefaults(m.deployCfg)
	return []string{
		"--image-name", d.ImageName,
		"--cluster-name", d.ClusterName,
		"--subnet-name", d.SubnetName,
		"--num-sockets", strconv.Itoa(d.NumSockets),
		"--cores-per-socket", strconv.Itoa(d.CoresPerSocket),
		"--memory-gib", strconv.Itoa(d.MemoryGiB),
		"--disk-gib", strconv.Itoa(d.DiskGiB),
		"--vm-user", d.VMUser,
		"--vm-password", d.VMPassword,
	}
}
