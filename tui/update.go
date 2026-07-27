package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// While a modal form is open, route messages to it so huh's internal
	// navigation/submit messages (nextFieldMsg, nextGroupMsg, cursor blink…)
	// reach the form. Background/data messages still fall through to the
	// handlers below so polling and streams keep working underneath the modal.
	if m.form != nil {
		switch msg.(type) {
		case tickMsg, spinner.TickMsg, progress.FrameMsg,
			connectedMsg, localOllaFoundMsg, statusMsg, modelsMsg, vmsMsg,
			chatEvMsg, pullEvMsg, procEvMsg, nextNameMsg,
			sshResultMsg, endpointsMsg, notifyMsg, tea.WindowSizeMsg,
			hubDialedMsg, hubEvMsg, hubDeployedMsg, nanoclawInstancesMsg,
			agentRemovedMsg:
			// handled normally below
		default:
			return m.updateForm(msg)
		}
	}

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.applyLayout(msg.Width, msg.Height)
		if !m.ready {
			m.ready = true
			// Don't open the Connect form on top of an in-flight local probe;
			// firstRunMsg opens it if the probe comes back empty.
			if m.gateway == "" && m.form == nil && !m.probingLocal {
				return m, m.openConnect()
			}
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case progress.FrameMsg:
		pm, cmd := m.prog.Update(msg)
		m.prog = pm.(progress.Model)
		return m, cmd

	case tickMsg:
		cmds := []tea.Cmd{tickCmd()}
		// Keep polling whenever we have a client, even while marked
		// disconnected, so a transient outage (e.g. Olla restarting to
		// re-discover models after a pull) auto-recovers instead of leaving
		// the TUI stuck OFFLINE until the user reconnects by hand.
		if m.client != nil {
			cmds = append(cmds, statusCmd(m.client))
		}
		return m, tea.Batch(cmds...)

	case firstRunMsg:
		m.probingLocal = false
		m.notice = "welcome — enter your Olla gateway URL and SSH credentials to get started"
		return m, m.openConnect()

	case localOllaFoundMsg:
		m.probingLocal = false
		m.gateway = msg.gateway
		// Persist so later runs connect straight away, and so the SSH creds
		// captured later attach to this gateway.
		_ = saveConnect(m.tokFile, msg.gateway, m.sshUser, m.sshPass)
		m.notice = "found a local Olla gateway — connecting to " + msg.gateway
		return m, connectCmd(msg.gateway)

	case connectedMsg:
		if msg.err != nil {
			m.connected = false
			m.connInfo = "connect failed: " + msg.err.Error()
			m.notice = "connect failed"
			return m, nil
		}
		m.connected = true
		m.gateway = msg.gateway
		m.client = NewOllaClient(msg.gateway)
		m.connVer = strings.TrimSpace(fmt.Sprintf("%s %s %s", msg.info.Name, msg.info.Version, msg.info.Edition))
		m.connInfo = m.connVer
		m.prevTime = time.Time{}
		m.notice = "connected to " + msg.gateway
		cmds := []tea.Cmd{statusCmd(m.client), modelsCmd(m.client)}
		if h := hostFromURL(msg.gateway); h != "" && m.sshPass != "" {
			cmds = append(cmds, endpointsCmd(h, orDefault(m.sshUser, "rocky"), m.sshPass))
			cmds = append(cmds, keyInstallCmd(h, orDefault(m.sshUser, "rocky"), m.sshPass))
		}
		return m, tea.Batch(cmds...)

	case statusMsg:
		if msg.err != nil {
			m.connected = false
			m.connInfo = "lost connection: " + msg.err.Error()
			return m, nil
		}
		wasDown := !m.connected
		m.connected = true
		m.applyStatus(msg.st)
		if wasDown {
			// A poll succeeded after an outage (e.g. Olla finished restarting):
			// restore the banner and refresh models/endpoints we may have missed.
			m.connInfo = orDefault(m.connVer, "connected to "+m.gateway)
			cmds := []tea.Cmd{modelsCmd(m.client)}
			if h := hostFromURL(m.gateway); h != "" && m.sshPass != "" {
				cmds = append(cmds, endpointsCmd(h, orDefault(m.sshUser, "rocky"), m.sshPass))
			}
			return m, tea.Batch(cmds...)
		}
		return m, nil

	case modelsMsg:
		if msg.err == nil {
			m.models = msg.models
			m.refreshModels()
		}
		return m, nil

	case vmsMsg:
		// Placement lists are fetched independently of the VM list, so keep
		// them even if the VM query itself failed (the deploy form needs them).
		if len(msg.clusters) > 0 {
			m.clusters = msg.clusters
		}
		if len(msg.images) > 0 {
			m.images = msg.images
		}
		if len(msg.subnets) > 0 {
			m.subnets = msg.subnets
		}
		// Drop saved placement the live PC doesn't list (e.g. a stale "canucks"
		// cluster from another environment) so deploy forms never offer it.
		if m.prunePlacement() {
			_ = saveDeployPC(m.tokFile, m.deployCfg, m.pcOver)
		}
		if msg.err != nil {
			m.notice = "Prism Central query failed: " + msg.err.Error()
			return m, nil
		}
		if msg.placementErr != nil {
			m.notice = "PC inventory query failed (deploy dropdowns unavailable): " + msg.placementErr.Error()
		}
		if len(msg.imageByID) > 0 {
			m.imageByID = msg.imageByID
		}
		m.vms = msg.vms
		m.refreshVMs()
		m.refreshPool()
		return m, nil

	case chatEvMsg:
		return m.handleChat(ChatEvent(msg))

	case pullEvMsg:
		return m.handlePull(PullEvent(msg))

	case procEvMsg:
		return m.handleProc(ProcEvent(msg))

	case nextNameMsg:
		if msg.err == nil {
			m.notice = "next worker name: " + msg.name
		} else {
			m.notice = "next-name failed: " + msg.err.Error()
		}
		return m, nil

	case sshResultMsg:
		if msg.err != nil {
			m.notice = "FAILED: " + msg.err.Error()
			return m, nil
		}
		m.notice = "OK: " + msg.msg
		var cmds []tea.Cmd
		if m.client != nil {
			cmds = append(cmds, statusCmd(m.client))
		}
		if h := hostFromURL(m.gateway); h != "" && m.sshPass != "" {
			cmds = append(cmds, endpointsCmd(h, orDefault(m.sshUser, "rocky"), m.sshPass))
		}
		return m, tea.Batch(cmds...)

	case endpointsMsg:
		if msg.err == nil {
			m.endpoints = msg.eps
			return m, m.modelsRefresh()
		}
		m.notice = "read endpoints failed: " + msg.err.Error()
		return m, nil

	case consoleReadyMsg:
		if msg.err != nil {
			m.notice = "key auth setup skipped (" + msg.err.Error() + ") — may prompt for password"
		}
		if msg.local {
			if msg.agent != "" {
				return m, localLaunchRegisterCmd(msg.cmd, orDefault(msg.label, "agent"), msg.agent, msg.host)
			}
			return m, localLaunchCmd(msg.cmd, orDefault(msg.label, "agent"))
		}
		if msg.cmd != "" {
			if msg.agent != "" {
				return m, sshLaunchRegisterCmd(msg.user, msg.host, msg.key, msg.cmd, orDefault(msg.label, "agent"), msg.agent)
			}
			return m, sshLaunchCmd(msg.user, msg.host, msg.key, msg.cmd, orDefault(msg.label, "agent"))
		}
		return m, sshConsoleCmd(msg.user, msg.host, msg.key)

	case agentRegisteredMsg:
		if m.agentReg == nil {
			m.agentReg = map[string]string{}
		}
		if m.agentHosts == nil {
			m.agentHosts = map[string][]string{}
		}
		m.agentReg[msg.agent] = msg.host
		if msg.host != "" && !containsStr(m.agentHosts[msg.agent], msg.host) {
			m.agentHosts[msg.agent] = append(m.agentHosts[msg.agent], msg.host)
		}
		_ = saveAgentReg(m.tokFile, m.agentReg, m.agentHosts)
		m.refreshAgents()
		m.notice = msg.agent + " registered on " + msg.host
		return m, nil

	case hubDialedMsg:
		if msg.gen != m.hubGen {
			// A reconnect superseded this dial while it was in flight.
			if msg.conn != nil {
				msg.conn.Close()
			}
			return m, nil
		}
		m.hubBusy = false
		if msg.err != nil {
			m.notice = "hub connect failed: " + msg.err.Error() + " — ctrl+d deploys the hub on the gateway"
			m.hubFeed = append(m.hubFeed, hubLine{sys: true, text: "connect failed: " + msg.err.Error()})
			m.renderHub()
			return m, nil
		}
		m.hubConn = msg.conn
		m.hubCh = msg.ch
		return m, waitHub(m.hubCh, m.hubGen)

	case hubEvMsg:
		if msg.gen != m.hubGen {
			return m, nil // event from a stale connection
		}
		return m.handleHubEvent(msg.ev)

	case hubDeployedMsg:
		m.hubBusy = false
		if msg.err != nil {
			m.notice = "hub deploy failed: " + msg.err.Error()
			return m, nil
		}
		m.notice = "agent hub deployed on the gateway — connecting…"
		return m, m.connectHub()

	case nanoclawInstancesMsg:
		m.nanoInstBusy = false
		m.nanoInst = msg.rows
		m.nanoInstErrs = msg.errs
		m.nanoInstAt = time.Now()
		// The inventory is ground truth: hosts that answered with zero
		// containers are no longer deployments, so drop them from the registry.
		m.reconcileNanoclawHosts(msg)
		if m.pendingRemove != "" {
			// This fetch was the first half of a remove: open the picker now
			// that the instance list is fresh.
			m.pendingRemove = ""
			return m, m.openNanoclawRemove()
		}
		if m.pendingConnect {
			// This fetch was the first half of an open: offer the connect picker
			// now that the instance list is fresh.
			m.pendingConnect = false
			return m, m.openNanoclawConnect()
		}
		if len(msg.errs) > 0 {
			m.notice = fmt.Sprintf("nanoclaw: %d instance(s) found, %d host(s) failed", len(msg.rows), len(msg.errs))
		} else {
			m.notice = fmt.Sprintf("nanoclaw: %d instance(s) across %d host(s)", len(msg.rows), msg.hosts)
		}
		return m, nil

	case agentRemovedMsg:
		if msg.container {
			// Refresh the inventory; its handler reconciles the registration
			// against what is actually left on the workers.
			notice := fmt.Sprintf("removed %d nanoclaw instance(s)", msg.removed)
			if len(msg.errs) > 0 {
				notice += " — failed: " + strings.Join(msg.errs, "; ")
			}
			m.notice = notice
			return m, m.fetchNanoclawInstances()
		}
		if len(msg.okHosts) > 0 {
			var kept []string
			for _, h := range m.agentHosts[msg.agent] {
				if !containsStr(msg.okHosts, h) {
					kept = append(kept, h)
				}
			}
			if len(kept) == 0 {
				delete(m.agentHosts, msg.agent)
				delete(m.agentReg, msg.agent)
			} else {
				m.agentHosts[msg.agent] = kept
				if !containsStr(kept, m.agentReg[msg.agent]) {
					m.agentReg[msg.agent] = kept[0]
				}
			}
			_ = saveAgentReg(m.tokFile, m.agentReg, m.agentHosts)
			m.refreshAgents()
		}
		if len(msg.errs) > 0 {
			m.notice = fmt.Sprintf("%s removed on %d host(s) — failed: %s", msg.agent, msg.removed, strings.Join(msg.errs, "; "))
		} else {
			m.notice = fmt.Sprintf("%s removed on %d host(s)", msg.agent, msg.removed)
		}
		return m, nil

	case notifyMsg:
		m.notice = string(msg)
		var cmds []tea.Cmd
		if m.client != nil {
			cmds = append(cmds, statusCmd(m.client))
		}
		if c := m.modelsRefresh(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	f, cmd := m.form.Update(msg)
	if ff, ok := f.(*huh.Form); ok {
		m.form = ff
	}
	switch m.form.State {
	case huh.StateCompleted:
		c := m.onFormComplete()
		m.form = nil
		m.modal = modalNone
		return m, tea.Batch(cmd, c)
	case huh.StateAborted:
		m.form = nil
		m.modal = modalNone
		m.notice = "cancelled"
		return m, cmd
	}
	return m, cmd
}

// ---- layout ----------------------------------------------------------------

func (m *model) applyLayout(w, h int) {
	m.width, m.height = w, h

	sidebarW := 24
	cardW := w - sidebarW - 1
	if cardW < 34 {
		cardW = 34
	}
	m.contentW = cardW - 6 // borders + horizontal padding
	if m.contentW < 24 {
		m.contentW = 24
	}

	headerH := 2
	helpH := 2
	if m.help.ShowAll {
		helpH = 7
	}
	m.contentH = h - headerH - helpH - 4 // borders + vertical padding + spacing
	if m.contentH < 6 {
		m.contentH = 6
	}

	listH := maxInt(m.contentH-3, 3)
	m.modelsList.SetSize(m.contentW, listH)
	m.poolList.SetSize(m.contentW, listH)
	m.agentsList.SetSize(m.contentW, listH)

	// Size the VM list to show 4 items per page. The default delegate is 3 lines
	// per item (height 2 + spacing 1); add 2 for the status bar + pagination.
	const vmRows = 4
	vmsWant := vmRows*3 + 2
	vmsH := clampInt(vmsWant, 7, maxInt(m.contentH-4, 7))
	m.vmsList.SetSize(m.contentW, vmsH)
	m.lockVMsPaging()

	// The Update list shares the layout with its Output pane, like Nutanix.
	m.updateList.SetSize(m.contentW, vmsH)
	// The custom-deploy submenu also shares the Nutanix layout (list + Output).
	m.customList.SetSize(m.contentW, vmsH)

	m.composer.SetWidth(m.contentW)
	chatH := maxInt(m.contentH-6, 3)
	m.chatVP.Width = m.contentW
	m.chatVP.Height = chatH
	m.logVP.Width = m.contentW
	m.logVP.Height = maxInt(m.contentH-3-vmsH-2, 3)

	// Hub: header + peers + hint + 2-line input around the feed viewport.
	m.hubTA.SetWidth(m.contentW)
	m.hubVP.Width = m.contentW
	m.hubVP.Height = maxInt(m.contentH-7, 3)

	m.prog.Width = clampInt(m.contentW-20, 12, 56)
	m.help.Width = w
	m.glam = newGlamour(m.contentW)
	m.renderChat()
	m.renderLog()
	m.renderHub()
}

// ---- key handling ----------------------------------------------------------

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()

	if k == "ctrl+c" {
		return m, tea.Quit
	}
	// "?" toggles help only when we're not typing (chat composer / list filter),
	// otherwise it must reach the textarea as a literal character.
	if k == "?" && !m.isTyping() {
		m.help.ShowAll = !m.help.ShowAll
		m.applyLayout(m.width, m.height)
		return m, nil
	}

	// Quick section jump (unless we're typing or filtering a list). '0' selects
	// the tenth section, so all sections stay reachable by number.
	if !m.isTyping() && len(k) == 1 && (k[0] >= '1' && k[0] <= '9' || k[0] == '0') {
		idx := int(k[0] - '1')
		if k[0] == '0' {
			idx = 9
		}
		if idx < len(sections) {
			m.section = section(idx)
			return m, m.enterContent()
		}
	}

	if m.zone == zoneSidebar {
		return m.handleSidebarKey(k)
	}
	return m.handleContentKey(msg, k)
}

func (m *model) isTyping() bool {
	if m.zone == zoneContent && (m.section == secChat || m.section == secHub) {
		return true
	}
	if l := m.activeList(); l != nil && l.FilterState() == list.Filtering {
		return true
	}
	return false
}

func (m *model) activeList() *list.Model {
	switch m.section {
	case secPool:
		return &m.poolList
	case secModels:
		return &m.modelsList
	case secNutanix:
		if m.nutanixCustom {
			return &m.customList
		}
		return &m.vmsList
	case secAgents:
		return &m.agentsList
	case secUpdate:
		return &m.updateList
	}
	return nil
}

func (m *model) enterContent() tea.Cmd {
	m.zone = zoneContent
	if m.section == secChat {
		m.hubTA.Blur()
		return m.composer.Focus()
	}
	if m.section == secHub {
		m.composer.Blur()
		// Dial the hub lazily on first entry so the channel is live by the
		// time the user starts typing.
		return tea.Batch(m.hubTA.Focus(), m.hubAutoConnect())
	}
	m.composer.Blur()
	m.hubTA.Blur()
	return nil
}

func (m *model) leaveContent() {
	m.zone = zoneSidebar
	m.composer.Blur()
	m.hubTA.Blur()
}

func (m *model) disconnect() {
	m.connected = false
	m.client = nil
	m.connInfo = "disconnected"
	m.notice = "disconnected"
}

// prunePlacement clears any saved cluster/subnet/image that the live Prism
// Central inventory doesn't contain, so the deploy/settings forms only ever
// offer values that actually exist here. Returns true if anything changed.
func (m *model) prunePlacement() bool {
	changed := false
	if len(m.clusters) > 0 && m.deployCfg.ClusterName != "" && !containsStr(m.clusters, m.deployCfg.ClusterName) {
		m.deployCfg.ClusterName = ""
		changed = true
	}
	if len(m.subnets) > 0 && m.deployCfg.SubnetName != "" && !containsStr(m.subnets, m.deployCfg.SubnetName) {
		m.deployCfg.SubnetName = ""
		changed = true
	}
	if len(m.images) > 0 && m.deployCfg.ImageName != "" && !containsStr(m.images, m.deployCfg.ImageName) {
		m.deployCfg.ImageName = ""
		changed = true
	}
	return changed
}

// modelsRefresh refreshes the model inventory from the workers directly when
// their URLs are known (immediate, accurate), else from the gateway tag cache.
func (m model) modelsRefresh() tea.Cmd {
	if workers := workersFromEndpoints(m.endpoints); len(workers) > 0 {
		return workerModelsCmd(workers)
	}
	if m.client != nil {
		return modelsCmd(m.client)
	}
	return nil
}

func (m *model) refreshAll() tea.Cmd {
	var cmds []tea.Cmd
	if m.client != nil {
		cmds = append(cmds, statusCmd(m.client), modelsCmd(m.client))
	}
	if m.pcCfg != nil {
		cmds = append(cmds, vmsCmd(m.pcCfg))
	}
	m.notice = "refreshing…"
	return tea.Batch(cmds...)
}

func (m model) handleSidebarKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k":
		m.section = section((int(m.section) - 1 + len(sections)) % len(sections))
	case "down", "j":
		m.section = section((int(m.section) + 1) % len(sections))
	case "enter", "right", "l", "tab":
		return m, m.enterContent()
	case "c":
		return m, m.openConnect()
	case "d":
		m.disconnect()
	case "r":
		return m, m.refreshAll()
	case "q":
		return m, tea.Quit
	}
	return m, nil
}

func (m model) handleContentKey(msg tea.KeyMsg, k string) (tea.Model, tea.Cmd) {
	// A filtering list owns every keystroke.
	if l := m.activeList(); l != nil && l.FilterState() == list.Filtering {
		nl, cmd := l.Update(msg)
		*l = nl
		return m, cmd
	}

	switch m.section {
	case secChat:
		switch k {
		case "esc":
			m.leaveContent()
			return m, nil
		case "enter":
			return m, m.sendChat()
		case "ctrl+n":
			m.newChatSession()
			return m, nil
		default:
			var cmd tea.Cmd
			m.composer, cmd = m.composer.Update(msg)
			return m, cmd
		}

	case secDash:
		switch k {
		case "esc", "left", "h":
			m.leaveContent()
		case "c":
			return m, m.openConnect()
		case "r":
			return m, m.refreshAll()
		}
		return m, nil

	case secLoad:
		if k == "esc" || k == "left" || k == "h" {
			m.leaveContent()
		}
		return m, nil

	case secAccess:
		switch k {
		case "esc", "left", "h":
			m.leaveContent()
		case "t":
			m.createToken()
		case "X":
			m.clearToken()
		}
		return m, nil

	case secPool:
		switch k {
		case "esc", "left", "h":
			m.leaveContent()
			return m, nil
		case "a":
			return m, m.openEndpoint()
		case "x", "delete":
			return m, m.removeSelectedEndpoint()
		case "s":
			return m, m.consoleSelectedWorker()
		case "S":
			return m, m.consoleGateway()
		case "r":
			var cmds []tea.Cmd
			if m.client != nil {
				cmds = append(cmds, statusCmd(m.client))
			}
			if h := hostFromURL(m.gateway); h != "" && m.sshPass != "" {
				cmds = append(cmds, endpointsCmd(h, orDefault(m.sshUser, "rocky"), m.sshPass))
			}
			return m, tea.Batch(cmds...)
		}
		nl, cmd := m.poolList.Update(msg)
		m.poolList = nl
		return m, cmd

	case secModels:
		switch k {
		case "esc", "left", "h":
			m.leaveContent()
			return m, nil
		case "p":
			return m, m.openPull()
		case "b":
			return m, m.openCatalog()
		case "x", "delete":
			return m, m.deleteSelectedModel()
		case "s":
			return m, m.setDefaultModel()
		case "enter":
			m.setChatModelFromList()
			return m, nil
		case "r":
			return m, m.modelsRefresh()
		}
		nl, cmd := m.modelsList.Update(msg)
		m.modelsList = nl
		return m, cmd

	case secAgents:
		switch k {
		case "esc", "left", "h":
			m.leaveContent()
			return m, nil
		case "enter", "o":
			return m, m.openSelectedAgent()
		case "d":
			return m, m.deploySelectedAgent()
		case "e":
			return m, m.openHermesCfg()
		case "i":
			return m, m.fetchNanoclawInstances()
		case "x", "delete":
			return m, m.removeSelectedAgent()
		case "r":
			if h := hostFromURL(m.gateway); h != "" && m.sshPass != "" {
				return m, endpointsCmd(h, orDefault(m.sshUser, "rocky"), m.sshPass)
			}
			return m, nil
		}
		nl, cmd := m.agentsList.Update(msg)
		m.agentsList = nl
		return m, cmd

	case secHub:
		switch k {
		case "esc":
			m.leaveContent()
			return m, nil
		case "enter":
			return m, m.sendHub()
		case "ctrl+r":
			return m, m.connectHub()
		case "ctrl+d":
			return m, m.deployHub()
		default:
			var cmd tea.Cmd
			m.hubTA, cmd = m.hubTA.Update(msg)
			return m, cmd
		}

	case secNutanix:
		if m.nutanixCustom {
			switch k {
			case "esc", "left", "h":
				m.nutanixCustom = false
				return m, nil
			case "enter":
				return m, m.deploySelectedCustom()
			case "x", "delete":
				m.deleteSelectedCustom()
				return m, nil
			case "b":
				if m.lastCustomAccess != "" {
					if err := openBrowser(m.lastCustomAccess); err != nil {
						m.notice = "could not open browser: " + err.Error()
					} else {
						m.notice = "opening " + m.lastCustomAccess
					}
				} else {
					m.notice = "no workload link yet — deploy a custom type with a port first"
				}
				return m, nil
			}
			nl, cmd := m.customList.Update(msg)
			m.customList = nl
			return m, cmd
		}
		switch k {
		case "esc", "left", "h":
			m.leaveContent()
			return m, nil
		case "g":
			return m, m.openDeploy("gateway")
		case "w":
			return m, m.openDeploy("worker")
		case "c":
			m.nutanixCustom = true
			return m, nil
		case "o":
			return m, m.startLocalOlla()
		case "e":
			return m, m.openNutanixCfg()
		case "x", "delete":
			return m, m.deleteSelectedVM()
		case "s":
			return m, m.consoleSelectedVM()
		case "n":
			if m.pcCfg != nil {
				return m, nextNameCmd(m.pcCfg, "worker")
			}
			return m, nil
		case "r":
			if m.pcCfg != nil {
				return m, vmsCmd(m.pcCfg)
			}
			return m, nil
		}
		nl, cmd := m.vmsList.Update(msg)
		m.vmsList = nl
		return m, cmd

	case secUpdate:
		switch k {
		case "esc", "left", "h":
			m.leaveContent()
			return m, nil
		case "enter":
			return m, m.runSelectedUpdate()
		}
		nl, cmd := m.updateList.Update(msg)
		m.updateList = nl
		return m, cmd
	}
	return m, nil
}

// ---- chat ------------------------------------------------------------------

// Session memory: every request replays the recent conversation so the model
// keeps context across turns, bounded so long sessions can't overflow the
// model's context window. Ctrl+N starts a fresh session.
const (
	chatHistoryMaxTurns = 24
	chatHistoryMaxChars = 24000
)

// chatMessages converts the session history (which already includes the
// just-appended user turn) into the OpenAI messages payload, keeping only the
// most recent turns within the turn/char budgets.
func (m *model) chatMessages() []ChatMessage {
	turns := m.history
	if len(turns) > chatHistoryMaxTurns {
		turns = turns[len(turns)-chatHistoryMaxTurns:]
	}
	// Walk back from the newest turn and cut before the turn that exceeds the
	// char budget (that turn is dropped too). The latest turn always survives,
	// even if it alone is over budget.
	start, chars := 0, 0
	for i := len(turns) - 1; i >= 0; i-- {
		chars += len(turns[i].content)
		if chars > chatHistoryMaxChars && i < len(turns)-1 {
			start = i + 1
			break
		}
	}
	msgs := make([]ChatMessage, 0, len(turns)-start)
	for _, t := range turns[start:] {
		role := "user"
		if t.role == roleBot {
			role = "assistant"
		}
		msgs = append(msgs, ChatMessage{Role: role, Content: t.content})
	}
	return msgs
}

// newChatSession clears the conversation (and its per-session stats) so the
// next prompt starts with no prior context.
func (m *model) newChatSession() {
	if m.streaming {
		m.notice = "wait for the current reply to finish before starting a new session"
		return
	}
	if len(m.history) == 0 && m.partial == "" {
		m.notice = "already a fresh session"
		return
	}
	m.history = nil
	m.partial = ""
	m.lastTTFT, m.lastTokS = 0, 0
	m.chatTokens, m.chatTotalTokens = 0, 0
	m.renderChat()
	m.notice = "new chat session — previous context cleared"
}

func (m *model) sendChat() tea.Cmd {
	if m.client == nil {
		m.notice = "connect to a gateway first"
		return nil
	}
	text := strings.TrimSpace(m.composer.Value())
	if text == "" {
		return nil
	}
	mdl := m.chatModel
	if mdl == "" {
		mdl = m.defaultModel()
	}
	if mdl == "" {
		m.notice = "no models available in the pool"
		return nil
	}
	m.composer.SetValue("")
	m.history = append(m.history, chatTurn{role: roleUser, content: text})
	m.partial = ""
	m.streaming = true
	m.chatTokens = 0
	m.chatTotalTokens = 0
	m.chatModelUsed = mdl
	m.chatStart = time.Now()
	m.chatFirst = time.Time{}
	m.chatCh = make(chan ChatEvent, 64)
	msgs := m.chatMessages()
	urls := extractURLs(text)
	client, ch := m.client, m.chatCh
	go func() {
		// Web fetch happens in the stream goroutine so the UI keeps spinning;
		// fetched pages are injected as a system message ahead of the prompt.
		if len(urls) > 0 {
			ch <- ChatEvent{Kind: "note", Content: fmt.Sprintf("web: fetching %d page(s)…", len(urls))}
			ctx, note := webContext(urls)
			if ctx != "" {
				last := msgs[len(msgs)-1]
				msgs = append(msgs[:len(msgs)-1], ChatMessage{Role: "system", Content: ctx}, last)
			}
			if note != "" {
				ch <- ChatEvent{Kind: "note", Content: note}
			}
		}
		client.ChatStream(mdl, msgs, ch)
	}()
	m.renderChat()
	return waitChat(m.chatCh)
}

func (m model) handleChat(ev ChatEvent) (tea.Model, tea.Cmd) {
	switch ev.Kind {
	case "first":
		m.chatFirst = time.Now()
		m.lastTTFT = float64(m.chatFirst.Sub(m.chatStart).Milliseconds())
		m.partial += ev.Content
		m.chatTokens++
	case "delta":
		m.partial += ev.Content
		m.chatTokens++
	case "usage":
		if ev.Usage != nil {
			if ev.Usage.CompletionTokens > 0 {
				m.chatTokens = ev.Usage.CompletionTokens
			}
			if ev.Usage.TotalTokens > 0 {
				m.chatTotalTokens = ev.Usage.TotalTokens
			}
		}
	case "note":
		// Progress from the web-fetch phase; shown in the status line.
		m.notice = ev.Content
	case "error":
		m.partial += "\n\n*error: " + errStr(ev.Err) + "*"
		m.finishChat()
		m.renderChat()
		return m, nil
	case "done":
		m.finishChat()
		m.renderChat()
		return m, nil
	}
	m.renderChat()
	return m, waitChat(m.chatCh)
}

func (m *model) finishChat() {
	end := time.Now()
	base := m.chatStart
	if !m.chatFirst.IsZero() {
		base = m.chatFirst
	}
	dt := end.Sub(base).Seconds()
	if dt > 0 && m.chatTokens > 0 {
		m.lastTokS = float64(m.chatTokens) / dt
	}
	if strings.TrimSpace(m.partial) != "" {
		m.history = append(m.history, chatTurn{role: roleBot, content: m.partial})
	}
	m.partial = ""
	m.streaming = false
	m.recordChatUsage()
}

// recordChatUsage attributes the just-finished chat's tokens to its model in the
// 30-day ledger and refreshes the dashboard aggregate. Prefers exact usage
// (prompt+completion) and falls back to the streamed token count.
func (m *model) recordChatUsage() {
	toks := m.chatTotalTokens
	if toks == 0 {
		toks = m.chatTokens
	}
	if toks <= 0 || m.chatModelUsed == "" {
		return
	}
	led := recordUsage(usagePath(m.tokFile), m.chatModelUsed, toks)
	m.usageAgg = usage30(led)
}

func (m model) defaultModel() string {
	if len(m.models) > 0 {
		return m.models[0].Name
	}
	return ""
}

// ---- pull ------------------------------------------------------------------

func (m *model) startPull(name string) tea.Cmd {
	// Olla is a load balancer and returns 501 ("model management operations not
	// supported by proxy") for /api/pull, so pulls must hit each worker's Ollama
	// directly. Fan out to the whole pool (same path as delete/set-default).
	workers := workersFromEndpoints(m.endpoints)
	if len(workers) == 0 {
		m.notice = "no workers known yet — open Pool and press r"
		return nil
	}
	return m.startMultiPull([]string{name}, workers, false, "pulling "+name+" across workers…")
}

func (m model) handlePull(ev PullEvent) (tea.Model, tea.Cmd) {
	if m.multiPull {
		return m.handleMultiPull(ev)
	}
	if ev.Err != nil {
		m.pulling = false
		m.pullStat = "pull failed: " + errStr(ev.Err)
		return m, nil
	}
	var cmds []tea.Cmd
	if ev.Total > 0 {
		m.pullFrac = float64(ev.Completed) / float64(ev.Total)
		m.pullStat = fmt.Sprintf("%s  %s / %s", ev.Status,
			humanBytes(float64(ev.Completed)), humanBytes(float64(ev.Total)))
		cmds = append(cmds, m.prog.SetPercent(m.pullFrac))
	} else if ev.Status != "" {
		m.pullStat = ev.Status
	}
	if ev.Done {
		m.pulling = false
		m.pullFrac = 1
		m.pullStat = "pull complete: " + m.pullName
		cmds = append(cmds, m.prog.SetPercent(1))
		if c := m.modelsRefresh(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)
	}
	cmds = append(cmds, waitPull(m.pullCh))
	return m, tea.Batch(cmds...)
}

// handleMultiPull updates per-worker progress rows during a parallel download.
// The closed-channel sentinel (empty Worker + Done) finalizes the run.
func (m model) handleMultiPull(ev PullEvent) (tea.Model, tea.Cmd) {
	if ev.Worker == "" && ev.Done {
		failed := 0
		for _, r := range m.pullRows {
			if r.failed {
				failed++
			}
		}
		m.pulling = false
		m.multiPull = false
		m.pullFrac = 1
		if failed > 0 {
			m.pullStat = fmt.Sprintf("%s downloaded — %d worker(s) failed", m.pullName, failed)
		} else {
			m.pullStat = "downloaded " + m.pullName + " — refreshing gateway…"
		}
		cmds := []tea.Cmd{m.prog.SetPercent(1)}
		if c := m.modelsRefresh(); c != nil {
			cmds = append(cmds, c)
		}
		// Force Olla to re-discover so the new model is routable immediately
		// rather than after its periodic (5m) discovery scan.
		cmds = append(cmds, refreshGatewayCmd(hostFromURL(m.gateway), m.sshUser, m.sshPass))
		return m, tea.Batch(cmds...)
	}
	for i := range m.pullRows {
		if m.pullRows[i].worker != ev.Worker {
			continue
		}
		if ev.Err != nil {
			// One model failed on this worker; flag it but let the worker keep
			// going through any remaining models (final Done event still comes).
			m.pullRows[i].failed = true
			m.pullRows[i].stat = ev.Status
			break
		}
		if ev.Total > 0 {
			m.pullRows[i].frac = float64(ev.Completed) / float64(ev.Total)
		}
		if ev.Status != "" {
			m.pullRows[i].stat = ev.Status
		}
		if ev.Done {
			m.pullRows[i].done = true
			m.pullRows[i].frac = 1
		}
		break
	}
	// Aggregate the headline bar as the mean of per-worker fractions.
	var sum float64
	for _, r := range m.pullRows {
		sum += r.frac
	}
	if len(m.pullRows) > 0 {
		m.pullFrac = sum / float64(len(m.pullRows))
	}
	return m, tea.Batch(m.prog.SetPercent(m.pullFrac), waitPull(m.pullCh))
}

// ---- endpoints / models / vms actions --------------------------------------

func (m *model) removeSelectedEndpoint() tea.Cmd {
	it, ok := m.poolList.SelectedItem().(poolItem)
	if !ok {
		m.notice = "select an endpoint to remove"
		return nil
	}
	host := hostFromURL(m.gateway)
	m.notice = "removing " + it.name + " via SSH …"
	return sshRemoveCmd(host, orDefault(m.sshUser, "rocky"), m.sshPass, it.name)
}

// deleteSelectedModel removes the selected model from every worker (the gateway
// proxy alone would only delete it from one).
func (m *model) deleteSelectedModel() tea.Cmd {
	it, ok := m.modelsList.SelectedItem().(modelItem)
	if !ok {
		m.notice = "select a model to delete"
		return nil
	}
	workers := workersFromEndpoints(m.endpoints)
	if len(workers) == 0 {
		m.notice = "no workers known yet — open Pool and press r"
		return nil
	}
	// If the deleted model was the pool default, drop the stale default so a
	// later download/warm doesn't silently re-pull the model just removed.
	if m.defModel != "" && modelMatches(it.name, m.defModel) {
		m.defModel = ""
		_ = saveDefaultModel(m.tokFile, "")
	}
	m.notice = "deleting " + it.name + " on all workers…"
	// Refresh the inventory once deletion completes so the removed model leaves
	// the list immediately (otherwise it lingers and a subsequent download can
	// act on the stale selection).
	return tea.Sequence(deleteModelAllCmd(workers, it.name), m.modelsRefresh())
}

// setDefaultModel marks the selected model as the pool-wide default: it becomes
// the chat default, is persisted, and is pulled (if missing) + warmed on every
// worker so it's ready everywhere.
func (m *model) setDefaultModel() tea.Cmd {
	it, ok := m.modelsList.SelectedItem().(modelItem)
	if !ok {
		m.notice = "select a model to set as default"
		return nil
	}
	m.defModel = it.name
	m.chatModel = it.name
	_ = saveDefaultModel(m.tokFile, it.name)
	m.refreshModels()
	workers := workersFromEndpoints(m.endpoints)
	if len(workers) == 0 {
		m.notice = "default set to " + it.name + " (no workers to warm yet)"
		return nil
	}
	return m.startMultiPull([]string{it.name}, workers, true, "setting default "+it.name+" across workers…")
}

func (m *model) setChatModelFromList() {
	if it, ok := m.modelsList.SelectedItem().(modelItem); ok {
		m.chatModel = it.name
		m.notice = "chat model set to " + it.name
	}
}

// startMultiPull fans the download of one or more models out to every worker
// concurrently, so all workers download at the same time rather than one after
// another. Each worker pulls every requested model (in sequence on that worker)
// while all workers run in parallel; the UI renders a per-worker progress row.
// Per-worker/per-model failures are reported inline but don't abort the others.
// The overall run completes when the channel closes.
func (m *model) startMultiPull(models []string, workers []workerRef, warm bool, label string) tea.Cmd {
	m.pulling = true
	m.multiPull = true
	m.pullName = strings.Join(models, ", ")
	m.pullStat = label
	m.pullFrac = 0
	m.section = secModels
	m.pullRows = make([]pullRow, len(workers))
	for i, w := range workers {
		m.pullRows[i] = pullRow{worker: w.name, stat: "queued"}
	}
	m.pullCh = make(chan PullEvent, 256)
	ch := m.pullCh
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w workerRef) {
			defer wg.Done()
			for _, model := range models {
				if ollamaHasModel(w.url, model) {
					ch <- PullEvent{Worker: w.name, Status: model + " present", Completed: 1, Total: 1}
				} else if err := ollamaPullInto(w.url, model, w.name, ch); err != nil {
					ch <- PullEvent{Worker: w.name, Status: model + " failed: " + err.Error(), Err: err}
					continue
				}
				if warm {
					ch <- PullEvent{Worker: w.name, Status: "warming " + model}
					_ = ollamaWarm(w.url, model)
				}
			}
			ch <- PullEvent{Worker: w.name, Status: "done", Completed: 1, Total: 1, Done: true}
		}(w)
	}
	// Close the channel once every worker is done; the closed-channel sentinel
	// (empty Worker, Done) tells handlePull the whole run finished.
	go func() { wg.Wait(); close(ch) }()
	return tea.Batch(m.prog.SetPercent(0), waitPull(ch))
}

// ---- ssh console -----------------------------------------------------------

func (m *model) endpointHost(name string) string {
	for _, e := range m.endpoints {
		if e.Name == name {
			return hostFromURL(e.URL)
		}
	}
	return ""
}

func (m *model) consoleSelectedWorker() tea.Cmd {
	it, ok := m.poolList.SelectedItem().(poolItem)
	if !ok {
		m.notice = "select a worker to console into"
		return nil
	}
	host := m.endpointHost(it.name)
	if host == "" {
		m.notice = "unknown host for " + it.name + " — press r to refresh Pool"
		return nil
	}
	m.notice = "preparing ssh to " + host + "…"
	return prepConsoleCmd(m.sshUser, host, m.sshPass)
}

func (m *model) consoleGateway() tea.Cmd {
	host := hostFromURL(m.gateway)
	if host == "" {
		m.notice = "connect to a gateway first"
		return nil
	}
	m.notice = "preparing ssh to gateway " + host + "…"
	return prepConsoleCmd(m.sshUser, host, m.sshPass)
}

func (m *model) consoleSelectedVM() tea.Cmd {
	it, ok := m.vmsList.SelectedItem().(vmItem)
	if !ok {
		m.notice = "select a VM to console into"
		return nil
	}
	if it.ip == "" {
		m.notice = "no IP known for " + it.name
		return nil
	}
	m.notice = "preparing ssh to " + it.ip + "…"
	return prepConsoleCmd(m.sshUser, it.ip, m.sshPass)
}

// ---- agents ----------------------------------------------------------------

// refreshAgents rebuilds the Agents list, annotating each with its deployment
// registration (so deployed agents show as ✓ registered on their host).
func (m *model) refreshAgents() {
	items := make([]list.Item, 0, len(agentCatalog))
	for _, a := range agentCatalog {
		host, reg := m.agentReg[a.name]
		items = append(items, agentItem{
			name: a.name, cli: a.cli, target: a.target, endpoint: a.endpoint,
			desc: a.desc, canDeploy: a.deployable, registered: reg, regHost: host,
		})
	}
	m.agentsList.SetItems(items)
}

// openSelectedAgent launches an agent's CLI. Worker agents prompt for the target
// worker (or reuse the registered one); Crush goes straight to the gateway.
func (m *model) openSelectedAgent() tea.Cmd {
	it, ok := m.agentsList.SelectedItem().(agentItem)
	if !ok {
		m.notice = "select an agent"
		return nil
	}
	a, _ := agentByName(it.name)
	if a.container {
		// Nanoclaw is many isolated containers: refresh the live inventory, then
		// let the user pick which instance to open an interactive ncl session in
		// (rather than launching a single CLI on a host).
		if len(m.agentDeployedHosts(a.name)) == 0 {
			m.notice = a.name + " is not deployed anywhere yet — press d to deploy it on a worker"
			return nil
		}
		if m.nanoInstBusy {
			m.notice = "already querying nanoclaw instances — try again in a moment"
			return nil
		}
		m.pendingConnect = true
		return m.fetchNanoclawInstances()
	}
	if a.target == "worker" {
		if h, reg := m.agentReg[a.name]; reg && h != "" {
			return m.startAgent(a, "open", h)
		}
		return m.openAgentHostPick(a.name, "open")
	}
	return m.startAgent(a, "open", m.agentHost(a))
}

// deploySelectedAgent installs + onboards an agent. Worker agents prompt for the
// target worker so the user can choose where it lands.
func (m *model) deploySelectedAgent() tea.Cmd {
	it, ok := m.agentsList.SelectedItem().(agentItem)
	if !ok {
		m.notice = "select an agent"
		return nil
	}
	a, _ := agentByName(it.name)
	if !a.deployable {
		m.notice = a.name + " needs no deploy — press enter/o to launch it"
		return nil
	}
	// Agents on the local Olla host (e.g. after "install Olla here") run
	// directly, so no SSH password is needed for them.
	if m.sshPass == "" && !isLocalHost(m.agentHost(a)) {
		m.notice = "set an SSH password (reconnect) to deploy agents"
		return nil
	}
	if a.target == "worker" {
		return m.openAgentHostPick(a.name, "deploy")
	}
	return m.startAgent(a, "deploy", m.agentHost(a))
}

// agentDeployedHosts returns every host an agent is registered as deployed on.
func (m *model) agentDeployedHosts(name string) []string {
	hosts := append([]string(nil), m.agentHosts[name]...)
	if h := m.agentReg[name]; h != "" && !containsStr(hosts, h) {
		hosts = append(hosts, h)
	}
	return hosts
}

// fetchNanoclawInstances queries every host Nanoclaw was ever deployed to (in
// parallel) for its container inventory, feeding the instance panel in the
// Agents view.
func (m *model) fetchNanoclawInstances() tea.Cmd {
	if m.nanoInstBusy {
		return nil
	}
	hosts := m.agentDeployedHosts("Nanoclaw")
	if len(hosts) == 0 {
		m.notice = "Nanoclaw is not deployed anywhere yet — press d to deploy it on a worker"
		return nil
	}
	m.nanoInstBusy = true
	m.notice = fmt.Sprintf("listing nanoclaw instances on %d host(s)…", len(hosts))
	return nanoclawInstancesCmd(hosts, orDefault(m.sshUser, "rocky"), m.sshPass)
}

// removeSelectedAgent starts deleting a deployed agent. Host agents pick the
// install (host) to uninstall; Nanoclaw first refreshes its live container
// inventory, then offers a per-instance picker.
func (m *model) removeSelectedAgent() tea.Cmd {
	it, ok := m.agentsList.SelectedItem().(agentItem)
	if !ok {
		m.notice = "select an agent to remove"
		return nil
	}
	a, ok := agentByName(it.name)
	if !ok {
		return nil
	}
	hosts := m.agentDeployedHosts(a.name)
	if len(hosts) == 0 {
		m.notice = a.name + " is not registered as deployed — nothing to remove"
		return nil
	}
	if a.container {
		if m.nanoInstBusy {
			m.notice = "already querying nanoclaw instances — try again in a moment"
			return nil
		}
		m.pendingRemove = a.name
		return m.fetchNanoclawInstances()
	}
	return m.openAgentRemove(a, hosts)
}

// reconcileNanoclawHosts prunes the Nanoclaw deployment registry using a fresh
// instance inventory: any host that answered the query but has no containers
// left is dropped (unreachable hosts are kept — their state is unknown).
func (m *model) reconcileNanoclawHosts(msg nanoclawInstancesMsg) {
	if len(msg.okHosts) == 0 {
		return
	}
	hasInst := map[string]bool{}
	for _, r := range msg.rows {
		hasInst[r.host] = true
	}
	var kept []string
	changed := false
	for _, h := range m.agentDeployedHosts("Nanoclaw") {
		if containsStr(msg.okHosts, h) && !hasInst[h] {
			changed = true
			continue
		}
		kept = append(kept, h)
	}
	if !changed {
		return
	}
	if len(kept) == 0 {
		delete(m.agentHosts, "Nanoclaw")
		delete(m.agentReg, "Nanoclaw")
	} else {
		m.agentHosts["Nanoclaw"] = kept
		if !containsStr(kept, m.agentReg["Nanoclaw"]) {
			m.agentReg["Nanoclaw"] = kept[0]
		}
	}
	_ = saveAgentReg(m.tokFile, m.agentReg, m.agentHosts)
	m.refreshAgents()
}

// startAgent dispatches the open/deploy of an agent against a resolved host.
func (m *model) startAgent(a agentDef, act, host string) tea.Cmd {
	if host == "" {
		if a.target == "worker" {
			m.notice = "no workers known — open Pool and press r first"
		} else {
			m.notice = "connect to a gateway first"
		}
		return nil
	}
	local := isLocalHost(host)
	if act == "deploy" {
		where := host
		if local {
			where = "this host"
		}
		m.notice = fmt.Sprintf("deploying %s on %s…", a.name, where)
		if a.name == "Hermes" && m.hermesGatewayWanted() {
			m.notice = fmt.Sprintf("deploying %s + Telegram gateway on %s (unattended)…", a.name, where)
		}
		if a.container {
			m.notice = fmt.Sprintf("deploying %d %s container(s) on %s…", maxInt(m.agentInstances, 1), a.name, where)
		}
		script := m.agentDeployScript(a)
		if local {
			if a.name == "Crush" {
				return localCrushCmd(m.crushConfigJSON(), script, a.name+" deploy", a.name)
			}
			return localDeployAgentCmd(script, a.name, a.name+" deploy")
		}
		if a.name == "Crush" {
			return crushCmd(m.sshUser, host, m.sshPass, m.crushConfigJSON(), script, a.name+" deploy", a.name)
		}
		return deployAgentCmd(m.sshUser, host, m.sshPass, script, a.name, a.name+" deploy")
	}
	m.notice = fmt.Sprintf("opening %s on %s…", a.name, host)
	// Nanoclaw runs as multiple containers, so opening it is instance-scoped and
	// handled by openSelectedAgent (fetch inventory → connect picker), never here.
	if local {
		if a.name == "Crush" {
			return localCrushCmd(m.crushConfigJSON(), "", a.name, "")
		}
		return localLaunchCmd(agentOpenCmd(a), a.name)
	}
	if a.name == "Crush" {
		return crushCmd(m.sshUser, host, m.sshPass, m.crushConfigJSON(), "", a.name, "")
	}
	return prepAgentCmd(m.sshUser, host, m.sshPass, agentOpenCmd(a), a.name)
}

func (m *model) deleteSelectedVM() tea.Cmd {
	if m.pcCfg == nil {
		m.notice = "Prism Central not configured"
		return nil
	}
	if m.procBusy {
		m.notice = "a deploy/delete is already running"
		return nil
	}
	it, ok := m.vmsList.SelectedItem().(vmItem)
	if !ok {
		m.notice = "select a VM to delete"
		return nil
	}
	var cmds []tea.Cmd
	// If this VM is a worker registered with the gateway, deregister it from
	// Olla first so the gateway stops routing to a VM that's about to vanish.
	if it.role == "worker" && m.sshPass != "" {
		if ep := m.endpointForVM(it.name, it.ip); ep != "" {
			if h := hostFromURL(m.gateway); h != "" {
				m.notice = "removing endpoint " + ep + " from Olla, then deleting VM…"
				cmds = append(cmds, sshRemoveCmd(h, orDefault(m.sshUser, "rocky"), m.sshPass, ep))
			}
		}
	}
	cmds = append(cmds, m.startProc([]string{"delete", "--name", it.name}, "delete "+it.name))
	return tea.Batch(cmds...)
}

// endpointForVM finds the gateway endpoint that corresponds to a VM, matching by
// endpoint name or by the VM's IP in the endpoint URL.
func (m *model) endpointForVM(name, ip string) string {
	for _, e := range m.endpoints {
		if e.Name == name || (ip != "" && hostFromURL(e.URL) == ip) {
			return e.Name
		}
	}
	return ""
}

func (m *model) createToken() {
	m.token = "olla-" + randToken(24)
	_ = saveToken(m.tokFile, m.token)
	m.notice = "API token created and saved to " + m.tokFile
}

func (m *model) clearToken() {
	m.token = ""
	_ = saveToken(m.tokFile, "")
	m.notice = "API token cleared"
}

// ---- subprocess (deploy/delete) --------------------------------------------

func (m *model) startProc(args []string, label string) tea.Cmd {
	m.procBusy = true
	m.section = secNutanix
	m.logLines = append(m.logLines, ">>> "+label+": nutanix_olla_vm.py "+strings.Join(args, " "))
	m.renderLog()
	m.procCh = make(chan ProcEvent, 128)
	go RunVMScript(m.pcCfg, args, m.procCh)
	return waitProc(m.procCh)
}

// startLocalOlla installs Olla on the machine running the TUI (typically the
// Linux server the operator is SSH'd into), then connects to it on success. This
// is an alternative to provisioning a Nutanix VM — no Prism Central required.
func (m *model) startLocalOlla() tea.Cmd {
	if m.procBusy {
		m.notice = "a deploy/delete is already running"
		return nil
	}
	if !localOllaSupported() {
		m.notice = "installing Olla locally is Linux-only — run the TUI on the target server (e.g. over SSH)"
		return nil
	}
	m.procBusy = true
	m.localOllaPending = true
	m.section = secNutanix
	m.logLines = append(m.logLines, ">>> installing Olla on this server (sudo) — will connect on :"+LocalOllaPort+" when done")
	m.renderLog()
	m.procCh = make(chan ProcEvent, 128)
	go RunLocalOllaInstall(m.procCh)
	return waitProc(m.procCh)
}

// startWorkerBatch provisions `count` workers in parallel (pattern-b
// --no-register), then registers them with the gateway in a single batched pass
// (see handleBatchDone). Names auto-increment from `name` (or the next free
// ollama-worker-NN when blank).
func (m *model) startWorkerBatch(count int, model, name string) tea.Cmd {
	if m.procBusy {
		m.notice = "a deploy/delete is already running"
		return nil
	}
	base := name
	if base == "" {
		nm, err := NextName(m.pcCfg, "worker")
		if err != nil {
			m.notice = "could not compute worker names: " + err.Error()
			return nil
		}
		base = nm
	}
	names := nextWorkerNames(base, count)
	jobs := make([]vmJob, 0, len(names))
	for _, n := range names {
		args := []string{"pattern-b", "--no-register", "--model", model, "--olla-url", m.gateway, "--vm-name", n}
		args = append(args, m.deployFlags()...)
		jobs = append(jobs, vmJob{tag: n, args: args})
	}
	d := withDeployDefaults(m.deployCfg)
	m.batch = deployBatch{
		active:  true,
		phase:   1,
		gateway: m.gateway,
		vmUser:  d.VMUser,
		vmPass:  d.VMPassword,
		total:   len(names),
	}
	m.procBusy = true
	m.section = secNutanix
	m.logLines = append(m.logLines,
		fmt.Sprintf(">>> deploying %d workers in parallel (%s … %s); will register as a batch when done",
			len(names), names[0], names[len(names)-1]))
	m.renderLog()
	m.procCh = make(chan ProcEvent, 256)
	go RunVMScripts(m.pcCfg, jobs, m.procCh)
	return waitProc(m.procCh)
}

// handleBatchDone advances a multi-worker deploy across its two phases: after
// parallel provisioning it kicks off the single batched registration; after
// registration it finalizes and refreshes the inventory.
func (m model) handleBatchDone(ev ProcEvent) (tea.Model, tea.Cmd) {
	if m.batch.phase == 1 {
		if len(m.batch.endpoints) == 0 {
			m.batch = deployBatch{}
			m.procBusy = false
			m.logLines = append(m.logLines, fmt.Sprintf("<<< multi-worker deploy failed: no workers provisioned (rc=%d)", ev.Code))
			m.notice = "multi-worker deploy failed — see Output"
			m.renderLog()
			if m.pcCfg != nil {
				return m, vmsCmd(m.pcCfg)
			}
			return m, nil
		}
		m.batch.phase = 2
		if ev.Code != 0 {
			m.logLines = append(m.logLines, fmt.Sprintf(">>> some workers failed (rc=%d); registering the %d that succeeded", ev.Code, len(m.batch.endpoints)))
		} else {
			m.logLines = append(m.logLines, fmt.Sprintf(">>> all workers provisioned; registering %d endpoint(s) with the gateway", len(m.batch.endpoints)))
		}
		m.renderLog()
		regArgs := []string{"register-endpoints", "--olla-url", m.batch.gateway,
			"--vm-user", m.batch.vmUser, "--vm-password", m.batch.vmPass}
		for _, ep := range m.batch.endpoints {
			regArgs = append(regArgs, "--endpoint",
				fmt.Sprintf("name=%s,url=%s,type=%s,priority=%d", ep.Name, ep.URL, ep.Type, ep.Priority))
		}
		m.procCh = make(chan ProcEvent, 64)
		// cfg=nil: register-endpoints does no Prism work and rejects --prism-url.
		go RunVMScript(nil, regArgs, m.procCh)
		return m, waitProc(m.procCh)
	}

	// phase 2 (registration) finished — finalize.
	n := len(m.batch.endpoints)
	m.batch = deployBatch{}
	m.procBusy = false
	if ev.Code == 0 {
		m.logLines = append(m.logLines, "<<< multi-worker deploy complete")
		m.notice = fmt.Sprintf("deployed and registered %d worker(s)", n)
	} else {
		m.logLines = append(m.logLines, fmt.Sprintf("<<< batch registration failed (rc=%d)", ev.Code))
		m.notice = fmt.Sprintf("workers deployed but registration failed (rc=%d)", ev.Code)
	}
	m.renderLog()
	var cmds []tea.Cmd
	if m.pcCfg != nil {
		cmds = append(cmds, vmsCmd(m.pcCfg))
	}
	if m.client != nil {
		cmds = append(cmds, statusCmd(m.client))
	}
	if h := hostFromURL(m.gateway); h != "" && m.sshPass != "" {
		cmds = append(cmds, endpointsCmd(h, orDefault(m.sshUser, "rocky"), m.sshPass))
	}
	return m, tea.Batch(cmds...)
}

// parseBatchEndpoint extracts an endpoint from an "OILSAND_ENDPOINT <json>" log
// line (the tag prefix, if any, is ignored).
func parseBatchEndpoint(line string) (batchEndpoint, bool) {
	const marker = "OILSAND_ENDPOINT "
	i := strings.Index(line, marker)
	if i < 0 {
		return batchEndpoint{}, false
	}
	var ep batchEndpoint
	if err := json.Unmarshal([]byte(line[i+len(marker):]), &ep); err != nil || ep.Name == "" || ep.URL == "" {
		return batchEndpoint{}, false
	}
	if ep.Type == "" {
		ep.Type = "ollama"
	}
	if ep.Priority == 0 {
		ep.Priority = 100
	}
	return ep, true
}

// parseVMRecord extracts a deploy-time "OILSAND_VM {name,image,ip}" line emitted
// by provision_vm, so the TUI can attribute the source image (and IP) to each VM
// it deploys.
func parseVMRecord(line string) (name, image, ip string, ok bool) {
	const marker = "OILSAND_VM "
	i := strings.Index(line, marker)
	if i < 0 {
		return "", "", "", false
	}
	var rec struct {
		Name  string `json:"name"`
		Image string `json:"image"`
		IP    string `json:"ip"`
	}
	if err := json.Unmarshal([]byte(line[i+len(marker):]), &rec); err != nil || rec.Name == "" {
		return "", "", "", false
	}
	return rec.Name, rec.Image, rec.IP, true
}

func (m model) handleProc(ev ProcEvent) (tea.Model, tea.Cmd) {
	if ev.Line != "" {
		m.logLines = append(m.logLines, ev.Line)
		if len(m.logLines) > 1000 {
			m.logLines = m.logLines[len(m.logLines)-1000:]
		}
		m.renderLog()
		if m.batch.active && m.batch.phase == 1 {
			if ep, ok := parseBatchEndpoint(ev.Line); ok {
				m.batch.endpoints = append(m.batch.endpoints, ep)
			}
		}
		if name, image, ip, ok := parseVMRecord(ev.Line); ok {
			if m.vmImages == nil {
				m.vmImages = map[string]string{}
			}
			m.vmImages[name] = image
			_ = saveVMImages(m.tokFile, m.vmImages)
			// For a custom deploy with a workload port, surface a clickable link.
			if m.pendingCustom != nil {
				if url := m.pendingCustom.accessURL(ip); url != "" {
					m.lastCustomAccess = url
					m.lastCustomName = name
					m.logLines = append(m.logLines, "ACCESS  "+name+"  →  "+osc8(url, url)+"  (click, or press b)")
					m.renderLog()
				}
			}
		}
	}
	if ev.Done {
		if m.batch.active {
			return m.handleBatchDone(ev)
		}
		m.procBusy = false
		m.pendingCustom = nil
		wasLocalOlla := m.localOllaPending
		m.localOllaPending = false
		if ev.Code == 0 {
			m.logLines = append(m.logLines, "<<< done")
			if wasLocalOlla {
				gw := normalizeGateway(LocalOllaURL())
				m.gateway = gw
				_ = saveConnect(m.tokFile, gw, m.sshUser, m.sshPass)
				m.notice = "Olla installed locally — connecting to " + gw
				m.renderLog()
				return m, connectCmd(gw)
			}
			m.notice = "deploy/delete finished"
		} else {
			m.logLines = append(m.logLines, fmt.Sprintf("<<< failed (rc=%d)", ev.Code))
			if wasLocalOlla {
				m.notice = fmt.Sprintf("local Olla install failed (rc=%d) — see Output; passwordless sudo may be required", ev.Code)
			} else {
				m.notice = fmt.Sprintf("deploy/delete failed (rc=%d)", ev.Code)
			}
		}
		m.renderLog()
		if m.pcCfg != nil && !wasLocalOlla {
			return m, vmsCmd(m.pcCfg)
		}
		return m, nil
	}
	return m, waitProc(m.procCh)
}

// ---- metrics + list refresh ------------------------------------------------

func (m *model) applyStatus(st Status) {
	m.status = st
	now := time.Now()
	totalReq := st.System.TotalRequests
	totalBytes := parseBytes(st.System.TotalTraffic)
	if !m.prevTime.IsZero() {
		dt := now.Sub(m.prevTime).Seconds()
		if dt > 0 {
			m.reqPerS = maxf(float64(totalReq-m.prevReq), 0) / dt
			m.bytesPerS = maxf(totalBytes-m.prevBytes, 0) / dt
			for _, ep := range st.Endpoints {
				prev := m.prevEpReq[ep.Name]
				m.epDelta[ep.Name] = maxf(float64(ep.Requests-prev), 0) / dt
			}
		}
	}
	m.prevTime = now
	m.prevReq = totalReq
	m.prevBytes = totalBytes
	for _, ep := range st.Endpoints {
		m.prevEpReq[ep.Name] = ep.Requests
	}
	m.reqHistory = append(m.reqHistory, m.reqPerS)
	if len(m.reqHistory) > 120 {
		m.reqHistory = m.reqHistory[len(m.reqHistory)-120:]
	}
	m.refreshPool()
}

func (m *model) refreshPool() {
	eps := append([]Endpoint(nil), m.status.Endpoints...)
	sort.Slice(eps, func(i, j int) bool { return eps[i].Name < eps[j].Name })
	items := make([]list.Item, 0, len(eps))
	for _, e := range eps {
		items = append(items, poolItem{
			name: e.Name, status: e.Status, models: e.Models.Count,
			prio: e.Priority, reqs: e.Requests, conns: e.Connections,
			latency: e.AvgLatency,
			image:   m.imageForVM(e.Name, hostFromURL(e.URL)),
		})
	}
	m.poolList.SetItems(items)
}

func (m *model) refreshModels() {
	items := make([]list.Item, 0, len(m.models))
	for _, md := range m.models {
		items = append(items, modelItem{
			name: md.Name, family: md.Details.Family, params: md.Details.ParameterSize,
			quant: md.Details.QuantizationLevel, size: humanBytes(float64(md.Size)),
			isDefault: m.defModel != "" && modelMatches(md.Name, m.defModel),
		})
	}
	m.modelsList.SetItems(items)
}

func (m *model) refreshVMs() {
	prefixes := m.customVMPrefixes()
	items := make([]list.Item, 0, len(m.vms))
	for _, v := range m.vms {
		role := v.Role
		if role != "gateway" && role != "worker" {
			// Surface user-defined custom-deploy VMs as managed too; skip every
			// other unrelated VM in Prism Central.
			if matchesCustomPrefix(v.Name, prefixes) {
				role = "custom"
			} else {
				continue
			}
		}
		items = append(items, vmItem{
			name: v.Name, role: role, power: v.Power, ip: v.IP,
			vcpu: v.VCPU, mem: v.MemGiB, disk: v.DiskGiB,
			image: m.imageForVM(v.Name, v.IP),
		})
	}
	m.vmsList.SetItems(items)
	m.lockVMsPaging()
}

// imageForVM resolves the source image of a VM, preferring the deploy-time
// record we persisted, then the live PC dataSource reference (by name/IP match).
func (m *model) imageForVM(name, ip string) string {
	if img := m.vmImages[name]; img != "" {
		return img
	}
	for _, v := range m.vms {
		if (name != "" && v.Name == name) || (ip != "" && v.IP == ip) {
			if v.ImageExtID != "" {
				if n := m.imageByID[v.ImageExtID]; n != "" {
					return n
				}
			}
			if img := m.vmImages[v.Name]; img != "" {
				return img
			}
		}
	}
	return ""
}

// customVMPrefixes returns the VM-name prefixes that identify custom-deploy VMs:
// one per saved deployment type (slug + "-") plus the helper's generic fallback.
func (m *model) customVMPrefixes() []string {
	prefixes := []string{"custom-"}
	for _, c := range m.customDeploys {
		prefixes = append(prefixes, slugifyName(c.Name)+"-")
	}
	return prefixes
}

func matchesCustomPrefix(name string, prefixes []string) bool {
	n := strings.ToLower(name)
	for _, p := range prefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// lockVMsPaging pins the Nutanix VM list to 4 items per page. SetSize/SetItems
// both recompute PerPage from the available height, so this must run after them.
func (m *model) lockVMsPaging() {
	m.vmsList.Paginator.PerPage = 4
	m.vmsList.Paginator.SetTotalPages(len(m.vmsList.Items()))
}
