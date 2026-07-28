package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

const pollInterval = 2 * time.Second

// ---- sections (left-hand navigation) ---------------------------------------

type section int

const (
	secDash section = iota
	secPool
	secModels
	secChat
	secAgents
	secBuzz
	secLoad
	secNutanix
	secAccess
	secUpdate
)

type sectionInfo struct {
	name string
	hint string
}

var sections = []sectionInfo{
	{"Dashboard", "live metrics & health"},
	{"Pool", "ollama endpoints"},
	{"Models", "models in the pool"},
	{"Chat", "test the pool"},
	{"Agents", "CLI agents via ssh"},
	{"Buzz", "Buzz relay & agent chat"},
	{"Load", "load balancing"},
	{"Nutanix", "VMs & deploy"},
	{"Access", "base URL & token"},
	{"Update", "maintenance & upgrades"},
}

// focus zone: are we navigating the sidebar, or interacting with the content?
type zone int

const (
	zoneSidebar zone = iota
	zoneContent
)

type modalKind int

const (
	modalNone modalKind = iota
	modalConnect
	modalEndpoint
	modalPull
	modalDeploy
	modalCatalog
	modalNutanixCfg
	modalAgentHost
	modalHermesCfg
	modalUpdateImage
	modalOSUpdate
	modalOllaKey
	modalUpdateAll
	modalCustomDeploy
	modalAgentRemove
	modalNanoConnect
)

type chatRole int

const (
	roleUser chatRole = iota
	roleBot
)

type chatTurn struct {
	role    chatRole
	content string
}

type model struct {
	// layout
	width, height int
	contentW      int // usable inner width of the content card
	contentH      int // usable inner height of the content card
	ready         bool

	// navigation
	section section
	zone    zone

	// connection
	gateway   string
	connected bool
	client    *OllaClient
	connInfo  string
	connVer   string // last known "Name Version Edition" banner, for reconnects
	sshUser   string
	sshPass   string

	// prism central
	pcCfg     *PCConfig
	deployCfg deploySettings // VM template + image used for new deploys
	pcOver    pcOverride     // persisted PC instance/account override (may be empty)

	// data
	status    Status
	models    []Model
	vms       []VM
	clusters  []string        // PC clusters available for placement
	images    []string        // PC disk images available to clone
	subnets   []string        // PC subnets available for placement
	endpoints []endpointEntry // cached gateway endpoints (for direct worker ops/console)

	// derived metrics
	prevReq    int
	prevBytes  float64
	prevTime   time.Time
	reqPerS    float64
	bytesPerS  float64
	reqHistory []float64
	prevEpReq  map[string]int
	epDelta    map[string]float64
	lastTokS   float64
	lastTTFT   float64

	// charm components
	km   keyMap
	help help.Model
	spin spinner.Model
	glam *glamour.TermRenderer
	prog progress.Model

	modelsList list.Model
	poolList   list.Model
	vmsList    list.Model
	agentsList list.Model
	updateList list.Model
	customList list.Model // user-defined custom deployment types (Nutanix submenu)

	// custom deployment types (Nutanix submenu)
	customDeploys []customDeploy
	nutanixCustom bool // Nutanix section is showing the custom-deploy submenu

	// custom-deploy access link: pendingCustom is the config of an in-flight
	// custom deploy; once its VM IP is reported we record lastCustomAccess.
	pendingCustom    *customDeploy
	lastCustomAccess string
	lastCustomName   string

	// image attribution: vmImages is the deploy-time VM name -> image map we
	// record; imageByID resolves a VM's source-image extId to a name from PC.
	vmImages  map[string]string
	imageByID map[string]string

	// agent registrations: agentReg holds the most recent deploy host per agent
	// (drives the ✓ badge / open-target), agentHosts every host it was ever
	// deployed on (drives updates for multi-worker agents like Nanoclaw).
	agentReg       map[string]string
	agentHosts     map[string][]string
	pendingAgent   string // agent awaiting host pick
	pendingAct     string // "open" | "deploy"
	agentInstances int    // container count for the next containerized-agent deploy
	pendingRemove  string // containerized agent whose instance list is being fetched for the remove picker
	pendingConnect bool   // Nanoclaw open: instance list is being fetched for the connect picker

	// nanoclaw instance overview (Agents tab, key i): all containers across
	// every worker Nanoclaw was ever deployed to.
	nanoInst     []nanoclawInstance
	nanoInstErrs []string
	nanoInstBusy bool
	nanoInstAt   time.Time

	// Buzz tab: deploy/poll the block/buzz relay on the gateway. hubGen
	// guards against stale poll ticks; buzzOpKey/channel come from deploy.
	hubGen        int
	hubOn         bool
	hubBusy       bool
	hubName       string
	hubPeers      []string
	hubFeed       []buzzLine
	hubVP         viewport.Model
	hubTA         textarea.Model
	buzzOpKey     string
	buzzChannelID string

	chatVP   viewport.Model
	logVP    viewport.Model
	composer textarea.Model

	// chat
	chatModel       string
	defModel        string // persisted pool-wide default model
	history         []chatTurn
	partial         string
	streaming       bool
	chatCh          chan ChatEvent
	chatStart       time.Time
	chatFirst       time.Time
	chatTokens      int
	chatTotalTokens int    // prompt+completion from usage (when reported)
	chatModelUsed   string // model of the in-flight chat (for usage attribution)

	// usage ledger (per-model tokens, 30-day rolling aggregate)
	usageAgg map[string]int64

	// pull
	pullCh   chan PullEvent
	pulling  bool
	pullName string
	pullStat string
	pullFrac float64
	// multi-worker parallel pull: one progress row per worker
	multiPull bool
	pullRows  []pullRow

	// proc (deploy/delete)
	procCh           chan ProcEvent
	procBusy         bool
	localOllaPending bool        // the running proc is a local Olla install; connect on success
	probingLocal     bool        // startup probe for a local Olla is in flight; hold off the Connect form
	batch            deployBatch // active multi-worker parallel deploy, if any
	logLines         []string

	// access
	token   string
	tokFile string

	// modal form (huh)
	modal modalKind
	form  *huh.Form
	// form-bound values
	fGateway string
	fSSHUser string
	fSSHPass string
	fEpName  string
	fEpURL   string
	fEpType  string
	fEpPrio  string
	fRole    string
	fName    string
	fModel   string
	fCount   string
	// nutanix settings form values
	fPCHost  string
	fPCPort  string
	fPCKey   string
	fPCUser  string
	fPCPass  string
	fImage   string
	fCluster string
	fSubnet  string
	fSockets string
	fCores   string
	fMem     string
	fDisk    string
	fVMUser  string
	fVMPass  string
	// agent host picker
	fAgentHost  string
	fAgentCount string // instances for containerized agents (Nanoclaw)
	// agent remove picker
	fRemoveTarget  string // "host" (host agents) · "host|container" (Nanoclaw) · "all"
	fRemoveVols    bool   // Nanoclaw: also delete the state volume(s)
	fConnectTarget string // Nanoclaw connect picker: "host|container"
	// hermes gateway / telegram settings form values
	fTgToken   string
	fTgAllowed string
	fTgHome    string
	fGwMode    string
	fGwEnable  bool
	// update section form values
	fUpdImage   string
	fSeedName   string
	fSeedURL    string
	fSeedPreset string
	fOSHosts    []string
	fOllaKey    string
	fOllaTarget string
	fUpdConfirm bool
	// custom deployment form values
	fCustName   string
	fCustURL    string
	fCustScheme string
	fCustPort   string
	fCustPath   string

	// hermes gateway / telegram config (persisted)
	hermesCfg hermesSettings

	// transient status line
	notice string
}

// ---- construction ----------------------------------------------------------

func newModel(gateway, sshUser, sshPass string) model {
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = lipgloss.NewStyle().Foreground(colAccent)

	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(colAccent).BorderForeground(colAccent)
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(colPrimary).BorderForeground(colAccent)
	delegate.Styles.NormalTitle = delegate.Styles.NormalTitle.Foreground(colText)
	delegate.Styles.NormalDesc = delegate.Styles.NormalDesc.Foreground(colMuted)

	mkList := func(title string) list.Model {
		l := list.New(nil, delegate, 30, 10)
		l.Title = title
		l.SetShowTitle(false)
		l.SetShowHelp(false)
		l.SetStatusBarItemName("item", "items")
		l.Styles.StatusBar = l.Styles.StatusBar.Foreground(colMuted)
		l.Styles.NoItems = l.Styles.NoItems.Foreground(colSubtle)
		return l
	}

	ta := textarea.New()
	ta.Placeholder = "Message the pool…  (Enter to send · Esc to menu)"
	ta.ShowLineNumbers = false
	ta.CharLimit = 4000
	ta.SetHeight(3)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.Prompt = lipgloss.NewStyle().Foreground(colAccent).Render("┃ ")

	hubTA := textarea.New()
	hubTA.Placeholder = "Message the agents…  (@name for a direct message)"
	hubTA.ShowLineNumbers = false
	hubTA.CharLimit = 4000
	hubTA.SetHeight(2)
	hubTA.FocusedStyle.CursorLine = lipgloss.NewStyle()
	hubTA.Prompt = lipgloss.NewStyle().Foreground(colAccent).Render("┃ ")

	prog := progress.New(progress.WithDefaultGradient(), progress.WithWidth(40))

	hp := help.New()
	hp.Styles.ShortKey = hp.Styles.ShortKey.Foreground(colCyan)
	hp.Styles.ShortDesc = hp.Styles.ShortDesc.Foreground(colMuted)
	hp.Styles.FullKey = hp.Styles.FullKey.Foreground(colCyan)
	hp.Styles.FullDesc = hp.Styles.FullDesc.Foreground(colMuted)

	home, _ := os.UserHomeDir()
	tokFile := filepath.Join(home, ".oilsand-ai-gateway", "tui.json")
	st := loadSettings(tokFile)

	// Pre-populate built-in custom deployments once. The seeded flag means
	// deleting them all later sticks (they won't silently reappear).
	customDeploys := st.CustomDeploys
	seedCustom := false
	if len(customDeploys) == 0 && !st.CustomSeeded {
		customDeploys = defaultCustomDeploys()
		seedCustom = true
	}
	vmImages := st.VMImages
	if vmImages == nil {
		vmImages = map[string]string{}
	}

	// Connection details come from flags/env first, then the values captured on
	// a previous launch (first-run setup), then the built-in user fallback.
	gateway = orDefault(gateway, st.Gateway)
	sshUser = orDefault(sshUser, st.SSHUser)
	sshPass = orDefault(sshPass, st.SSHPass)

	// Prism Central comes from the persisted override when set, else mcp.json.
	pcCfg := pcConfigFromOverride(st.PC)
	if pcCfg == nil {
		pcCfg = LoadPCConfig()
	}

	// Strip any old hardcoded lab placement (e.g. "canucks") carried over in
	// tui.json so it never shows as a default in a different environment.
	cleanedDeploy := dropLegacyPlacement(st.Deploy)
	if cleanedDeploy != st.Deploy {
		_ = saveDeployPC(tokFile, withDeployDefaults(cleanedDeploy), st.PC)
	}

	m := model{
		gateway:       normalizeGateway(gateway),
		sshUser:       orDefault(sshUser, "rocky"),
		sshPass:       sshPass,
		pcCfg:         pcCfg,
		deployCfg:     withDeployDefaults(cleanedDeploy),
		pcOver:        st.PC,
		km:            newKeyMap(),
		help:          hp,
		spin:          sp,
		glam:          newGlamour(80),
		prog:          prog,
		modelsList:    mkList("Models"),
		poolList:      mkList("Pool"),
		vmsList:       mkList("VMs"),
		agentsList:    mkList("Agents"),
		updateList:    mkList("Update"),
		customList:    mkList("Custom deployments"),
		chatVP:        viewport.New(80, 16),
		logVP:         viewport.New(80, 8),
		hubVP:         viewport.New(80, 12),
		composer:      ta,
		hubTA:         hubTA,
		prevEpReq:     map[string]int{},
		epDelta:       map[string]float64{},
		tokFile:       tokFile,
		token:         st.Token,
		defModel:      st.DefaultModel,
		usageAgg:      usage30(loadUsage(usagePath(tokFile))),
		agentReg:      st.Agents,
		agentHosts:    st.AgentHosts,
		hermesCfg:     st.Hermes,
		buzzOpKey:     st.Buzz.OperatorKey,
		buzzChannelID: st.Buzz.ChannelID,
		customDeploys: customDeploys,
		vmImages:      vmImages,
		imageByID:     map[string]string{},
	}
	if seedCustom {
		_ = saveCustomDeploys(tokFile, customDeploys)
	}
	if m.agentReg == nil {
		m.agentReg = map[string]string{}
	}
	if m.agentHosts == nil {
		m.agentHosts = map[string][]string{}
	}
	// Migrate configs saved before per-agent host lists existed.
	for a, h := range m.agentReg {
		if h != "" && len(m.agentHosts[a]) == 0 {
			m.agentHosts[a] = []string{h}
		}
	}
	if m.defModel != "" {
		m.chatModel = m.defModel
	}
	// With no gateway configured, Init() probes for a local Olla. Mark that in
	// flight so the first WindowSizeMsg doesn't open the Connect form over a
	// probe that is about to succeed.
	m.probingLocal = m.gateway == ""

	// The Update list is a dense menu (7 fixed actions), so give it a compact
	// single-line delegate — the default 3-line delegate would paginate after
	// ~3 rows in the available height.
	compact := list.NewDefaultDelegate()
	compact.ShowDescription = false
	compact.SetSpacing(0)
	compact.SetHeight(1)
	compact.Styles.SelectedTitle = compact.Styles.SelectedTitle.
		Foreground(colAccent).BorderForeground(colAccent)
	compact.Styles.NormalTitle = compact.Styles.NormalTitle.Foreground(colText)
	m.updateList.SetDelegate(compact)
	m.customList.SetDelegate(compact)

	m.refreshAgents()
	m.refreshUpdateList()
	m.refreshCustomList()
	return m
}

func newGlamour(width int) *glamour.TermRenderer {
	if width < 20 {
		width = 20
	}
	// Use a fixed dark style rather than WithAutoStyle: auto-style probes the
	// terminal background (an OSC/cursor-position query) every time this runs —
	// including on each window resize — and the reply leaks into focused inputs
	// over SSH / serial consoles.
	g, _ := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(width))
	return g
}

// ---- Bubble Tea: Init ------------------------------------------------------

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick, tickCmd()}
	if m.pcCfg != nil {
		cmds = append(cmds, vmsCmd(m.pcCfg))
	}
	if m.gateway != "" {
		cmds = append(cmds, connectCmd(m.gateway))
	} else {
		// Nothing configured yet. The TUI is often run on the gateway box
		// itself (it ships there with the deploy), so look for an Olla on this
		// machine before asking anyone to type a URL. The probe falls back to
		// firstRunMsg, which opens the Connect form as before.
		cmds = append(cmds, localOllaProbeCmd())
	}
	return tea.Batch(cmds...)
}

// ---- messages --------------------------------------------------------------

type tickMsg time.Time

// firstRunMsg is emitted once at startup when no gateway is configured and no
// local Olla was found, so the connection modal opens automatically for the
// user to enter their details.
type firstRunMsg struct{}

// localOllaFoundMsg reports that the startup probe found an Olla gateway
// running on this machine, so the TUI can connect to it without being
// configured by hand.
type localOllaFoundMsg struct{ gateway string }
type connectedMsg struct {
	gateway string
	info    VersionInfo
	err     error
}
type statusMsg struct {
	st  Status
	err error
}
type modelsMsg struct {
	models []Model
	err    error
}
type vmsMsg struct {
	vms          []VM
	clusters     []string
	images       []string
	subnets      []string
	imageByID    map[string]string // image extId -> name
	err          error
	placementErr error // non-nil if the cluster/image/subnet queries failed
}
type chatEvMsg ChatEvent
type pullEvMsg PullEvent

// pullRow tracks one worker's progress during a parallel multi-worker download.
type pullRow struct {
	worker string
	stat   string
	frac   float64
	done   bool
	failed bool
}
type procEvMsg ProcEvent
type nextNameMsg struct {
	name string
	err  error
}
type sshResultMsg struct {
	msg string
	err error
}
type endpointsMsg struct {
	eps []endpointEntry
	err error
}

// batchEndpoint is a worker endpoint emitted (as "OILSAND_ENDPOINT <json>") by a
// parallel `pattern-b --no-register` run, collected for batched registration.
type batchEndpoint struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Type     string `json:"type"`
	Priority int    `json:"priority"`
}

// deployBatch tracks a multi-worker parallel deploy: phase 1 provisions N workers
// concurrently (collecting their endpoints), phase 2 registers them all with the
// gateway in a single olla.yaml write to avoid races.
type deployBatch struct {
	active    bool
	phase     int // 1 = provisioning, 2 = registering
	gateway   string
	vmUser    string
	vmPass    string
	total     int
	endpoints []batchEndpoint
}
type consoleReadyMsg struct {
	user  string
	host  string
	key   string
	cmd   string // remote command to launch (empty = interactive login shell)
	label string
	agent string // non-empty: register this agent on clean exit
	local bool   // run cmd on this machine instead of over SSH
	err   error
}
type agentRegisteredMsg struct {
	agent string
	host  string
}
type notifyMsg string

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}
