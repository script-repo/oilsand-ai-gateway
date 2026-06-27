package main

import (
	"fmt"
	"strconv"
	"strings"

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

// selectOrInput returns a Huh select bound to val when opts is non-empty — so
// the user picks from the live Prism Central inventory — otherwise a free-text
// input, so the form still works when PC is unreachable or hasn't been polled.
func selectOrInput(title, placeholder string, opts []string, val *string) huh.Field {
	if len(opts) == 0 {
		return huh.NewInput().Title(title).Placeholder(placeholder).Value(val)
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
	return huh.NewSelect[string]().Title(title).Options(options...).Value(val)
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
		huh.NewInput().Title("Olla gateway URL").
			Placeholder("http://gateway-host:40114").Value(&m.fGateway),
		huh.NewInput().Title("SSH user").Description("used to edit endpoints on the gateway VM").
			Placeholder("rocky").Value(&m.fSSHUser),
		huh.NewInput().Title("SSH password").Password(true).Value(&m.fSSHPass),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme())
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
		huh.NewInput().Title("Endpoint name").Placeholder("ollama-worker-03").Value(&m.fEpName),
		huh.NewInput().Title("Endpoint URL").Placeholder("http://worker-host:11434").Value(&m.fEpURL),
		huh.NewSelect[string]().Title("Type").
			Options(huh.NewOptions("ollama", "openai", "vllm", "lmstudio")...).Value(&m.fEpType),
		huh.NewInput().Title("Priority").Placeholder("100").Value(&m.fEpPrio),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme())
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
		huh.NewInput().Title("Model to pull").
			Description("downloads through the gateway's Ollama proxy").
			Placeholder("llama3.2:3b").Value(&m.fModel),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme())
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
	)).WithWidth(formWidth(m)).WithHeight(18).WithShowHelp(true).WithTheme(huhTheme())
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
	m.fRole, m.fName, m.fModel = role, "", def
	m.modal = modalDeploy
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Role").
			Options(huh.NewOptions("worker", "gateway")...).Value(&m.fRole),
		huh.NewInput().Title("VM name").Description("blank = auto-increment (ollama-worker-NN)").Value(&m.fName),
		huh.NewInput().Title("Model").Description("worker only · defaults to the current default model").
			Placeholder(def).Value(&m.fModel),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme())
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
			huh.NewInput().Title("PC host / IP").Placeholder("prism-central host or IP").Value(&m.fPCHost),
			huh.NewInput().Title("PC port").Placeholder("9440").Value(&m.fPCPort),
			huh.NewInput().Title("API key").Password(true).
				Description("X-Ntnx-Api-Key (preferred)").Value(&m.fPCKey),
			huh.NewInput().Title("Service account user").Placeholder("admin").Value(&m.fPCUser),
			huh.NewInput().Title("Service account password").Password(true).Value(&m.fPCPass),
		),
		huh.NewGroup(
			huh.NewNote().Title("VM template").Description("Resources for newly deployed gateways/workers."),
			huh.NewInput().Title("Sockets").Placeholder("2").Value(&m.fSockets),
			huh.NewInput().Title("Cores / socket").Placeholder("4").Value(&m.fCores),
			huh.NewInput().Title("Memory (GiB)").Placeholder("12").Value(&m.fMem),
			huh.NewInput().Title("Disk (GiB)").Placeholder("50").Value(&m.fDisk),
			huh.NewInput().Title("VM user").Placeholder("rocky").Value(&m.fVMUser),
			huh.NewInput().Title("VM password").Password(true).Value(&m.fVMPass),
		),
		huh.NewGroup(
			huh.NewNote().Title("Image & placement").
				Description(fmt.Sprintf("Live from Prism Central: %d images, %d clusters, %d subnets. "+
					"If a count is 0, PC wasn't reachable — set the PC fields above, save, then reopen to get dropdowns.",
					len(m.images), len(m.clusters), len(m.subnets))),
			selectOrInput("Image name", "disk image to clone", m.images, &m.fImage),
			selectOrInput("Cluster", "target cluster", m.clusters, &m.fCluster),
			selectOrInput("Subnet", "target subnet", m.subnets, &m.fSubnet),
		),
	).WithWidth(formWidth(m)).WithHeight(18).WithShowHelp(true).WithTheme(huhTheme())
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
			huh.NewInput().Title("Bot token").Password(true).
				Placeholder("123456789:AA...").Value(&m.fTgToken),
			huh.NewInput().Title("Allowed Telegram user IDs").
				Description("comma-separated; blank = pair via DM after deploy").Value(&m.fTgAllowed),
			huh.NewInput().Title("Home channel (optional)").
				Description("default chat ID for cron/notifications").Value(&m.fTgHome),
			huh.NewSelect[string]().Title("Gateway service").
				Options(
					huh.NewOption("user (no sudo, runs at login via linger)", "user"),
					huh.NewOption("system (root, starts at boot)", "system"),
				).Value(&m.fGwMode),
			huh.NewConfirm().Title("Auto-setup gateway on Hermes deploy?").
				Affirmative("Yes").Negative("No").Value(&m.fGwEnable),
		),
	).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme())
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
	m.modal = modalAgentHost
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(verb + " " + agentName + " on which worker?").
			Options(opts...).Value(&m.fAgentHost),
	)).WithWidth(formWidth(m)).WithShowHelp(true).WithTheme(huhTheme())
	return m.form.Init()
}

// onFormComplete dispatches the action for the just-submitted modal.
func (m *model) onFormComplete() tea.Cmd {
	switch m.modal {
	case modalConnect:
		gw := normalizeGateway(m.fGateway)
		if gw == "" {
			m.notice = "enter a gateway URL"
			return nil
		}
		m.gateway = gw
		m.sshUser = orDefault(strings.TrimSpace(m.fSSHUser), "rocky")
		m.sshPass = m.fSSHPass
		m.connInfo = "connecting…"
		// Remember the connection so later launches skip first-run setup.
		_ = saveConnect(m.tokFile, m.gateway, m.sshUser, m.sshPass)
		return connectCmd(gw)

	case modalEndpoint:
		name := strings.TrimSpace(m.fEpName)
		url := strings.TrimSpace(m.fEpURL)
		if name == "" || url == "" {
			m.notice = "endpoint name and URL are required"
			return nil
		}
		prio, _ := strconv.Atoi(strings.TrimSpace(m.fEpPrio))
		if prio == 0 {
			prio = 100
		}
		e := endpointEntry{
			Name: name, URL: url,
			Type:          orDefault(m.fEpType, "ollama"),
			Priority:      prio,
			CheckInterval: "10s", CheckTimeout: "3s",
		}
		host := hostFromURL(m.gateway)
		m.notice = "adding " + name + " via SSH to " + host + " …"
		return sshAddAndKeyCmd(host, orDefault(m.sshUser, "rocky"), m.sshPass, e)

	case modalPull:
		name := strings.TrimSpace(m.fModel)
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
		if m.fRole == "gateway" {
			args := []string{"pattern-a"}
			args = append(args, m.deployFlags()...)
			if n := strings.TrimSpace(m.fName); n != "" {
				args = append(args, "--vm-name", n)
			}
			return m.startProc(args, "deploy gateway")
		}
		if m.gateway == "" {
			m.notice = "connect to the target Olla gateway first"
			return nil
		}
		args := []string{"pattern-b", "--model",
			orDefault(strings.TrimSpace(m.fModel), m.effDefaultModel()), "--olla-url", m.gateway}
		args = append(args, m.deployFlags()...)
		if n := strings.TrimSpace(m.fName); n != "" {
			args = append(args, "--vm-name", n)
		}
		return m.startProc(args, "deploy worker")

	case modalNutanixCfg:
		d := withDeployDefaults(deploySettings{
			ImageName:      strings.TrimSpace(m.fImage),
			ClusterName:    strings.TrimSpace(m.fCluster),
			SubnetName:     strings.TrimSpace(m.fSubnet),
			NumSockets:     atoiOr(m.fSockets, 0),
			CoresPerSocket: atoiOr(m.fCores, 0),
			MemoryGiB:      atoiOr(m.fMem, 0),
			DiskGiB:        atoiOr(m.fDisk, 0),
			VMUser:         strings.TrimSpace(m.fVMUser),
			VMPassword:     m.fVMPass,
		})
		o := pcOverride{
			Host:     strings.TrimSpace(m.fPCHost),
			Port:     strings.TrimSpace(m.fPCPort),
			APIKey:   strings.TrimSpace(m.fPCKey),
			User:     strings.TrimSpace(m.fPCUser),
			Password: m.fPCPass,
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
		return m.startAgent(a, m.pendingAct, strings.TrimSpace(m.fAgentHost))

	case modalHermesCfg:
		h := hermesSettings{
			TelegramBotToken:     strings.TrimSpace(m.fTgToken),
			TelegramAllowedUsers: strings.TrimSpace(m.fTgAllowed),
			TelegramHomeChannel:  strings.TrimSpace(m.fTgHome),
			GatewayEnabled:       m.fGwEnable,
			GatewayMode:          orDefault(m.fGwMode, "user"),
		}
		m.hermesCfg = h
		_ = saveHermesCfg(m.tokFile, h)
		m.refreshAgents()
		m.notice = "Hermes gateway settings saved"
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
