package main

import (
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
			connectedMsg, statusMsg, modelsMsg, vmsMsg,
			chatEvMsg, pullEvMsg, procEvMsg, nextNameMsg,
			sshResultMsg, endpointsMsg, notifyMsg, tea.WindowSizeMsg:
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
			if m.gateway == "" && m.form == nil {
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
		if m.connected && m.client != nil {
			cmds = append(cmds, statusCmd(m.client))
		}
		return m, tea.Batch(cmds...)

	case firstRunMsg:
		m.notice = "welcome — enter your Olla gateway URL and SSH credentials to get started"
		return m, m.openConnect()

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
		m.connInfo = strings.TrimSpace(fmt.Sprintf("%s %s %s", msg.info.Name, msg.info.Version, msg.info.Edition))
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
		m.applyStatus(msg.st)
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
		if msg.err != nil {
			m.notice = "Prism Central query failed: " + msg.err.Error()
			return m, nil
		}
		if msg.placementErr != nil {
			m.notice = "PC inventory query failed (deploy dropdowns unavailable): " + msg.placementErr.Error()
		}
		m.vms = msg.vms
		m.refreshVMs()
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
		m.agentReg[msg.agent] = msg.host
		_ = saveAgentReg(m.tokFile, m.agentReg)
		m.refreshAgents()
		m.notice = msg.agent + " registered on " + msg.host
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

	m.composer.SetWidth(m.contentW)
	chatH := maxInt(m.contentH-6, 3)
	m.chatVP.Width = m.contentW
	m.chatVP.Height = chatH
	m.logVP.Width = m.contentW
	m.logVP.Height = maxInt(m.contentH-3-vmsH-2, 3)

	m.prog.Width = clampInt(m.contentW-20, 12, 56)
	m.help.Width = w
	m.glam = newGlamour(m.contentW)
	m.renderChat()
	m.renderLog()
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

	// Quick section jump (unless we're typing or filtering a list).
	if !m.isTyping() && len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
		idx := int(k[0] - '1')
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
	if m.zone == zoneContent && m.section == secChat {
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
		return &m.vmsList
	case secAgents:
		return &m.agentsList
	}
	return nil
}

func (m *model) enterContent() tea.Cmd {
	m.zone = zoneContent
	if m.section == secChat {
		return m.composer.Focus()
	}
	m.composer.Blur()
	return nil
}

func (m *model) leaveContent() {
	m.zone = zoneSidebar
	m.composer.Blur()
}

func (m *model) disconnect() {
	m.connected = false
	m.client = nil
	m.connInfo = "disconnected"
	m.notice = "disconnected"
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
		case "r":
			if h := hostFromURL(m.gateway); h != "" && m.sshPass != "" {
				return m, endpointsCmd(h, orDefault(m.sshUser, "rocky"), m.sshPass)
			}
			return m, nil
		}
		nl, cmd := m.agentsList.Update(msg)
		m.agentsList = nl
		return m, cmd

	case secNutanix:
		switch k {
		case "esc", "left", "h":
			m.leaveContent()
			return m, nil
		case "g":
			return m, m.openDeploy("gateway")
		case "w":
			return m, m.openDeploy("worker")
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
	}
	return m, nil
}

// ---- chat ------------------------------------------------------------------

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
	go m.client.ChatStream(mdl, []ChatMessage{{Role: "user", Content: text}}, m.chatCh)
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
	if m.client == nil {
		m.notice = "connect to a gateway first"
		return nil
	}
	m.pulling = true
	m.pullName = name
	m.pullStat = "starting pull " + name
	m.pullFrac = 0
	m.section = secModels
	m.pullCh = make(chan PullEvent, 64)
	go m.client.PullModel(name, m.pullCh)
	return tea.Batch(m.prog.SetPercent(0), waitPull(m.pullCh))
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
	if m.sshPass == "" {
		m.notice = "set an SSH password (reconnect) to deploy agents"
		return nil
	}
	if a.target == "worker" {
		return m.openAgentHostPick(a.name, "deploy")
	}
	return m.startAgent(a, "deploy", m.agentHost(a))
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
	if act == "deploy" {
		m.notice = fmt.Sprintf("deploying %s on %s…", a.name, host)
		if a.name == "Hermes" && m.hermesGatewayWanted() {
			m.notice = fmt.Sprintf("deploying %s + Telegram gateway on %s (unattended)…", a.name, host)
		}
		script := m.agentDeployScript(a)
		if a.name == "Crush" {
			return crushCmd(m.sshUser, host, m.sshPass, m.crushConfigJSON(), script, a.name+" deploy", a.name)
		}
		return deployAgentCmd(m.sshUser, host, m.sshPass, script, a.name, a.name+" deploy")
	}
	m.notice = fmt.Sprintf("opening %s on %s…", a.name, host)
	if a.name == "Crush" {
		return crushCmd(m.sshUser, host, m.sshPass, m.crushConfigJSON(), "", a.name, "")
	}
	return prepAgentCmd(m.sshUser, host, m.sshPass, loginShell(a.cli), a.name)
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

func (m model) handleProc(ev ProcEvent) (tea.Model, tea.Cmd) {
	if ev.Line != "" {
		m.logLines = append(m.logLines, ev.Line)
		if len(m.logLines) > 1000 {
			m.logLines = m.logLines[len(m.logLines)-1000:]
		}
		m.renderLog()
	}
	if ev.Done {
		m.procBusy = false
		if ev.Code == 0 {
			m.logLines = append(m.logLines, "<<< done")
			m.notice = "deploy/delete finished"
		} else {
			m.logLines = append(m.logLines, fmt.Sprintf("<<< failed (rc=%d)", ev.Code))
			m.notice = fmt.Sprintf("deploy/delete failed (rc=%d)", ev.Code)
		}
		m.renderLog()
		if m.pcCfg != nil {
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
	items := make([]list.Item, 0, len(m.vms))
	for _, v := range m.vms {
		if v.Role != "gateway" && v.Role != "worker" {
			continue
		}
		items = append(items, vmItem{
			name: v.Name, role: v.Role, power: v.Power, ip: v.IP,
			vcpu: v.VCPU, mem: v.MemGiB, disk: v.DiskGiB,
		})
	}
	m.vmsList.SetItems(items)
	m.lockVMsPaging()
}

// lockVMsPaging pins the Nutanix VM list to 4 items per page. SetSize/SetItems
// both recompute PerPage from the available height, so this must run after them.
func (m *model) lockVMsPaging() {
	m.vmsList.Paginator.PerPage = 4
	m.vmsList.Paginator.SetTotalPages(len(m.vmsList.Items()))
}
