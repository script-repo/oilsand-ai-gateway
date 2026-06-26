package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"
)

func (m model) View() string {
	if !m.ready {
		return "loading…"
	}
	if m.form != nil {
		return m.viewModal()
	}
	header := m.viewHeader()
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.viewSidebar(), " ", m.viewContent())
	return lipgloss.JoinVertical(lipgloss.Left, header, body, m.viewHelp())
}

// ---- header ----------------------------------------------------------------

func (m model) viewHeader() string {
	brand := brandStyle.Render("OILSAND AI GATEWAY")

	var badges []string
	if m.connected {
		badges = append(badges, badgeOn.Render("● ONLINE"))
	} else {
		badges = append(badges, badgeOff.Render("○ OFFLINE"))
	}
	if m.pcCfg != nil {
		badges = append(badges, badgeInfo.Render("PC "+m.pcCfg.Host))
	} else {
		badges = append(badges, badgeDim.Render("PC n/a"))
	}
	if m.connected {
		badges = append(badges, badgeDim.Render(fmt.Sprintf("%d models", len(m.models))))
		badges = append(badges, badgeDim.Render(fmt.Sprintf("%d clients", m.status.System.ActiveConnections)))
	}
	right := strings.Join(badges, " ")

	info := m.connInfo
	if info == "" {
		info = "press c to connect"
	}
	left := brand + brandSubStyle.Render("  "+truncate(info, maxInt(m.width-lipgloss.Width(brand)-lipgloss.Width(right)-6, 6)))

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	bar := left + strings.Repeat(" ", gap) + right
	rule := dimStyle.Render(strings.Repeat("─", maxInt(m.width, 1)))
	return lipgloss.JoinVertical(lipgloss.Left, bar, rule)
}

// ---- sidebar ---------------------------------------------------------------

func (m model) viewSidebar() string {
	var b strings.Builder
	b.WriteString(navTitle.Render("≡  MENU"))
	b.WriteString("\n\n")
	for i, s := range sections {
		label := fmt.Sprintf("%d  %s", i+1, s.name)
		switch {
		case section(i) == m.section && m.zone == zoneSidebar:
			b.WriteString(navItemActive.Width(18).Render("▌ " + label))
		case section(i) == m.section:
			b.WriteString(navItemCurrent.Width(18).Render("▌ " + label))
		default:
			b.WriteString(navItem.Width(18).Render("  " + label))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(navHint.Render(truncate(sections[m.section].hint, 18)))

	box := sidebarBox
	if m.zone == zoneSidebar {
		box = box.BorderForeground(colAccent)
	}
	return box.Width(22).Height(m.contentH).MaxHeight(m.contentH + 4).Render(b.String())
}

// ---- content ---------------------------------------------------------------

func (m model) viewContent() string {
	title := sectionTitle.Render(sections[m.section].name)
	var sub string
	if m.zone == zoneContent {
		sub = accentText.Render("  ● focused — esc for menu")
	} else {
		sub = dimStyle.Render("  enter to focus")
	}
	inner := lipgloss.JoinVertical(lipgloss.Left, title+sub, "", m.sectionBody())

	outerCard := maxInt(m.width-25, 34)
	box := contentBox
	if m.zone == zoneContent {
		box = box.BorderForeground(colAccent)
	}
	return box.Width(outerCard - 2).Height(m.contentH).MaxHeight(m.contentH + 4).Render(inner)
}

func (m model) sectionBody() string {
	switch m.section {
	case secDash:
		return m.viewDash()
	case secPool:
		return m.viewPool()
	case secModels:
		return m.viewModels()
	case secChat:
		return m.viewChat()
	case secAgents:
		return m.viewAgents()
	case secLoad:
		return m.viewLoad()
	case secNutanix:
		return m.viewNutanix()
	case secAccess:
		return m.viewAccess()
	}
	return ""
}

// ---- section: dashboard ----------------------------------------------------

func (m model) viewDash() string {
	if !m.connected {
		return dimStyle.Render("Not connected to an Olla gateway.\n\nPress " +
			accentText.Render("c") + dimStyle.Render(" to connect, or set --gateway on launch."))
	}
	s := m.status.System
	cards := []string{
		statCardView("Clients", fmt.Sprintf("%d", s.ActiveConnections)),
		statCardView("Requests/s", fmt.Sprintf("%.1f", m.reqPerS)),
		statCardView("Throughput", humanBytes(m.bytesPerS)+"/s"),
		statCardView("Avg latency", emptyDash(s.AvgLatency)),
		statCardView("Success", emptyDash(s.SuccessRate)),
		statCardView("Endpoints", fmt.Sprintf("%v", s.EndpointsUp)),
		statCardView("Total req", fmt.Sprintf("%d", s.TotalRequests)),
		statCardView("Uptime", emptyDash(s.Uptime)),
	}
	perRow := maxInt(m.contentW/22, 1)
	grid := gridView(cards, perRow)

	line := accentText.Render(spark(m.reqHistory, minInt(m.contentW-12, 80)))
	trend := labelStyle.Render("req/s ") + line + dimStyle.Render(fmt.Sprintf("  now %.1f", m.reqPerS))
	return lipgloss.JoinVertical(lipgloss.Left, grid, "", trend, "", m.viewTokenUsage())
}

// viewTokenUsage renders the per-model token totals for the last 30 days.
func (m model) viewTokenUsage() string {
	rows := usage30Sorted(m.usageAgg)
	out := []string{labelStyle.Render("Tokens / model · last 30 days")}
	if len(rows) == 0 {
		out = append(out, dimStyle.Render("  no usage recorded yet — send a chat to start tracking"))
		return lipgloss.JoinVertical(lipgloss.Left, out...)
	}
	var maxTok, total int64
	for _, r := range rows {
		total += r.tokens
		if r.tokens > maxTok {
			maxTok = r.tokens
		}
	}
	barW := clampInt(m.contentW-40, 10, 36)
	limit := minInt(len(rows), 6)
	for i := 0; i < limit; i++ {
		r := rows[i]
		frac := 0.0
		if maxTok > 0 {
			frac = float64(r.tokens) / float64(maxTok)
		}
		st := lipgloss.NewStyle().Foreground(palette(i))
		out = append(out, fmt.Sprintf("%s %-20s %s %s",
			dot("online"), truncate(r.model, 20), barInline(frac, barW, st), humanCount(r.tokens)))
	}
	out = append(out, dimStyle.Render(fmt.Sprintf("  total %s tokens · %d model(s) · measured via this TUI",
		humanCount(total), len(rows))))
	return lipgloss.JoinVertical(lipgloss.Left, out...)
}

func statCardView(label, value string) string {
	return statCard.Render(statLabel.Render(label) + "\n" + statBig.Render(truncate(value, 16)))
}

func gridView(cards []string, perRow int) string {
	var rows []string
	for i := 0; i < len(cards); i += perRow {
		end := minInt(i+perRow, len(cards))
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, cards[i:end]...))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// ---- section: pool ---------------------------------------------------------

func (m model) viewPool() string {
	if !m.connected {
		return dimStyle.Render("Not connected. Press c on the Dashboard to connect first.")
	}
	hint := dimStyle.Render("a add · x remove · s ssh worker · S ssh gateway · / filter")
	return lipgloss.JoinVertical(lipgloss.Left, m.poolList.View(), hint)
}

// ---- section: models -------------------------------------------------------

func (m model) viewModels() string {
	if !m.connected {
		return dimStyle.Render("Not connected. Press c on the Dashboard to connect first.")
	}
	if m.multiPull {
		// The per-worker progress panel is multi-line; shrink the list so every
		// worker row fits inside the content box instead of being clipped.
		footer := m.viewMultiPull()
		m.modelsList.SetHeight(maxInt(m.contentH-lipgloss.Height(footer)-1, 3))
		return lipgloss.JoinVertical(lipgloss.Left, m.modelsList.View(), footer)
	}
	var footer string
	switch {
	case m.pulling:
		footer = m.spin.View() + " " + m.prog.ViewAs(m.pullFrac) + " " +
			warnStyle.Render(truncate(m.pullStat, maxInt(m.contentW-44, 8)))
	case m.pullStat != "":
		footer = goodStyle.Render("✓ ") + dimStyle.Render(m.pullStat)
	default:
		footer = dimStyle.Render("p pull · b browse · s set default · x delete (all) · enter chat model · / filter")
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.modelsList.View(), footer)
}

// viewMultiPull renders one progress row per worker for a parallel download so
// it's clear all workers are fetching simultaneously, not one at a time.
func (m model) viewMultiPull() string {
	head := fmt.Sprintf("%s downloading %s to %d workers (in parallel)",
		m.spin.View(), m.pullName, len(m.pullRows))
	lines := []string{warnStyle.Render(head)}
	nameW := 0
	for _, r := range m.pullRows {
		if len(r.worker) > nameW {
			nameW = len(r.worker)
		}
	}
	for _, r := range m.pullRows {
		line := fmt.Sprintf("%-*s  %s %3.0f%%  %s",
			nameW, r.worker, renderMiniBar(r.frac, 16), r.frac*100, r.stat)
		line = truncate(line, maxInt(m.contentW-2, 24))
		switch {
		case r.failed:
			line = badStyle.Render(line)
		case r.done:
			line = goodStyle.Render(line)
		default:
			line = dimStyle.Render(line)
		}
		lines = append(lines, line)
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

// renderMiniBar draws a fixed-width progress bar from a 0..1 fraction.
func renderMiniBar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(width))
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// ---- section: chat ---------------------------------------------------------

func (m model) viewChat() string {
	if !m.connected {
		return dimStyle.Render("Not connected. Press c on the Dashboard to connect first.")
	}
	stats := dimStyle.Render(fmt.Sprintf("TTFT %s · tok/s %s · model %s  ·  Agents tab → open in Crush",
		msIf(m.lastTTFT), tokIf(m.lastTokS), orDefault(m.chatModel, m.defaultModel())))
	return lipgloss.JoinVertical(lipgloss.Left, m.chatVP.View(), stats, m.composer.View())
}

// ---- section: agents -------------------------------------------------------

func (m model) viewAgents() string {
	gw := hostFromURL(m.gateway)
	ws := workersFromEndpoints(m.endpoints)
	worker := "(none — refresh Pool)"
	if len(ws) > 0 {
		worker = ws[0].host
	}
	loc := dimStyle.Render(fmt.Sprintf("  gateway %s · worker %s", orDefault(gw, "-"), worker))
	hint := dimStyle.Render("enter/o open (ssh + launch CLI) · d deploy · e Telegram/gateway · r refresh hosts · / filter")
	note := dimStyle.Render("Crush → Olla server (whole pool) · OpenClaw/Hermes → a worker's local Ollama")

	gwState := "off (deploy launches CLI) · e to configure"
	if m.hermesGatewayWanted() {
		mode := m.hermesCfg.gatewayMode()
		extra := ""
		if m.hermesCfg.TelegramAllowedUsers == "" {
			extra = " · pair via DM after deploy"
		}
		gwState = fmt.Sprintf("Telegram configured (%s service)%s — Hermes deploy is unattended", mode, extra)
	}
	hermesLine := dimStyle.Render("Hermes gateway: " + gwState)

	return lipgloss.JoinVertical(lipgloss.Left,
		labelStyle.Render("AI agents")+loc,
		m.agentsList.View(),
		note,
		hermesLine,
		hint,
	)
}

// ---- section: load ---------------------------------------------------------

func (m model) viewLoad() string {
	eps := append([]Endpoint(nil), m.status.Endpoints...)
	if len(eps) == 0 {
		return dimStyle.Render("No endpoints reported yet.\nConnect and send traffic (Chat tab) to watch the load\ndistribute across the Ollama workers.")
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].Requests > eps[j].Requests })

	var total int
	var maxConns, maxDelta float64
	for _, e := range eps {
		total += e.Requests
		if float64(e.Connections) > maxConns {
			maxConns = float64(e.Connections)
		}
		if m.epDelta[e.Name] > maxDelta {
			maxDelta = m.epDelta[e.Name]
		}
	}

	barW := clampInt(m.contentW-44, 10, 30)
	var rows []string
	rows = append(rows, labelStyle.Render("Request share (cumulative)"))
	for i, e := range eps {
		frac := 0.0
		if total > 0 {
			frac = float64(e.Requests) / float64(total)
		}
		st := lipgloss.NewStyle().Foreground(palette(i))
		leader := "  "
		if maxDelta > 0 && m.epDelta[e.Name] >= maxDelta {
			leader = accentText.Render("◀ busiest")
		}
		rows = append(rows, fmt.Sprintf("%s %-16s %s %5.1f%%  %drq %s",
			dot(e.Status), truncate(e.Name, 16), barInline(frac, barW, st), frac*100, e.Requests, leader))
	}

	rows = append(rows, "", labelStyle.Render("Active connections"))
	for i, e := range eps {
		frac := 0.0
		if maxConns > 0 {
			frac = float64(e.Connections) / maxConns
		}
		st := lipgloss.NewStyle().Foreground(palette(i))
		rows = append(rows, fmt.Sprintf("%s %-16s %s %d",
			dot(e.Status), truncate(e.Name, 16), barInline(frac, barW, st), e.Connections))
	}

	rows = append(rows, "",
		labelStyle.Render("Gateway throughput (req/s)"),
		accentText.Render(spark(m.reqHistory, minInt(m.contentW-12, 80)))+fmt.Sprintf("  now %.1f rps", m.reqPerS))
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// ---- section: nutanix ------------------------------------------------------

func (m model) viewNutanix() string {
	if m.pcCfg == nil {
		return dimStyle.Render("Prism Central is not configured.\nAdd PC_HOST / PC_API_KEY to the nutanix-v4-mcp entry in ~/.cursor/mcp.json.")
	}
	busy := dimStyle.Render("g gateway · w worker · e settings · x delete · s ssh · n next-name · r refresh")
	if m.procBusy {
		busy = m.spin.View() + " " + warnStyle.Render("running deploy/delete…")
	}
	clusters := ""
	if len(m.clusters) > 0 {
		clusters = dimStyle.Render("  clusters: " + strings.Join(m.clusters, ", "))
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		labelStyle.Render("Managed VMs")+clusters,
		m.vmsList.View(),
		labelStyle.Render("Output")+"  "+busy,
		m.logVP.View(),
	)
}

// ---- section: access -------------------------------------------------------

func (m model) viewAccess() string {
	base := "(connect to a gateway first)"
	if m.gateway != "" {
		base = m.gateway + "/olla/openai/v1"
	}
	mdl := orDefault(m.defaultModel(), "(no models discovered yet)")
	token := orDefault(m.token, "(none — press t to create)")
	exModel := orDefault(m.defaultModel(), "rnj-1:latest")
	exKey := orDefault(m.token, "olla")
	curl := fmt.Sprintf("curl %s/chat/completions \\\n  -H 'Authorization: Bearer %s' \\\n  -H 'Content-Type: application/json' \\\n  -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'",
		base, exKey, exModel)
	return lipgloss.JoinVertical(lipgloss.Left,
		labelStyle.Render("OpenAI-compatible Base URL"), "  "+base, "",
		labelStyle.Render("API key / token")+dimStyle.Render("   (t new · X clear)"), "  "+token,
		dimStyle.Render("  Olla Community does not enforce inbound keys; use for client configs."), "",
		labelStyle.Render("Model"), "  "+mdl, "",
		labelStyle.Render("Example"), codeStyle.Render("  "+strings.ReplaceAll(curl, "\n", "\n  ")),
	)
}

// ---- modal -----------------------------------------------------------------

func (m model) viewModal() string {
	titles := map[modalKind]string{
		modalConnect:    "Connect to Olla gateway",
		modalEndpoint:   "Add Ollama endpoint",
		modalPull:       "Pull a model",
		modalDeploy:     "Deploy Nutanix VM",
		modalCatalog:    "Browse & download models",
		modalNutanixCfg: "Nutanix settings",
		modalAgentHost:  "Choose worker",
		modalHermesCfg:  "Hermes gateway / Telegram",
	}
	inner := modalTitle.Render(titles[m.modal]) + "\n\n" + m.form.View()
	box := modalBox.Render(inner)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

// ---- help bar --------------------------------------------------------------

func (m model) viewHelp() string {
	var out string
	if m.help.ShowAll {
		out = m.help.FullHelpView(m.fullHelp())
	} else {
		out = m.help.ShortHelpView(m.shortHelp())
	}
	if m.notice != "" {
		return noticeStyle.Render(truncate(m.notice, maxInt(m.width-2, 10))) + "\n" + out
	}
	return out
}

func (m model) shortHelp() []key.Binding {
	tail := []key.Binding{m.km.Help, m.km.Quit}
	if m.zone == zoneSidebar {
		return append([]key.Binding{m.km.Up, m.km.Down, m.km.Open, m.km.Connect, m.km.Refresh}, tail...)
	}
	var mid []key.Binding
	switch m.section {
	case secChat:
		mid = []key.Binding{m.km.Send, m.km.Back}
	case secPool:
		mid = []key.Binding{m.km.Add, m.km.Remove, m.km.Console, m.km.ConsoleGW, m.km.Filter, m.km.Back}
	case secModels:
		mid = []key.Binding{m.km.Pull, m.km.Browse, m.km.Remove, m.km.SetDef, m.km.SetModel, m.km.Filter, m.km.Back}
	case secAgents:
		mid = []key.Binding{m.km.AgentOpen, m.km.AgentDeploy, m.km.EditCfg, m.km.Filter, m.km.Back}
	case secNutanix:
		mid = []key.Binding{m.km.Deploy, m.km.Worker, m.km.EditCfg, m.km.Delete, m.km.Console, m.km.NextName, m.km.Back}
	case secAccess:
		mid = []key.Binding{m.km.Token, m.km.ClearToken, m.km.Back}
	default:
		mid = []key.Binding{m.km.Back}
	}
	return append(mid, tail...)
}

func (m model) fullHelp() [][]key.Binding {
	nav := []key.Binding{m.km.Up, m.km.Down, m.km.Open, m.km.Back, m.km.Filter}
	actions := []key.Binding{
		m.km.Connect, m.km.Disconnect, m.km.Refresh, m.km.Add, m.km.Remove,
		m.km.Pull, m.km.Browse, m.km.SetDef, m.km.SetModel,
	}
	access := []key.Binding{m.km.Console, m.km.ConsoleGW, m.km.AgentOpen, m.km.AgentDeploy, m.km.Token, m.km.ClearToken}
	nutanix := []key.Binding{m.km.Deploy, m.km.Worker, m.km.EditCfg, m.km.Delete, m.km.NextName}
	global := []key.Binding{m.km.Send, m.km.Help, m.km.Quit}
	return [][]key.Binding{nav, actions, access, nutanix, global}
}

// ---- render helpers (chat / log) -------------------------------------------

func (m *model) renderChat() {
	var b strings.Builder
	if len(m.history) == 0 && m.partial == "" && !m.streaming {
		b.WriteString(dimStyle.Render("Ask the pool anything. Responses stream token-by-token.\nEnter sends · Esc returns to the menu."))
	}
	for _, t := range m.history {
		if t.role == roleUser {
			b.WriteString(youStyle.Render("❯ you") + "\n" + t.content + "\n\n")
		} else {
			b.WriteString(botStyle.Render("⬢ olla") + "\n" + m.md(t.content) + "\n")
		}
	}
	if m.streaming || strings.TrimSpace(m.partial) != "" {
		cur := ""
		if m.streaming {
			cur = accentText.Render("▌")
		}
		b.WriteString(botStyle.Render("⬢ olla") + "\n" + m.partial + cur + "\n")
	}
	m.chatVP.SetContent(b.String())
	m.chatVP.GotoBottom()
}

func (m *model) md(s string) string {
	if m.glam == nil {
		return s
	}
	out, err := m.glam.Render(s)
	if err != nil {
		return s
	}
	return strings.TrimRight(out, "\n")
}

func (m *model) renderLog() {
	if len(m.logLines) == 0 {
		m.logVP.SetContent(dimStyle.Render("(deploy/delete output appears here)"))
		return
	}
	m.logVP.SetContent(strings.Join(m.logLines, "\n"))
	m.logVP.GotoBottom()
}
