package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// DefaultModel mirrors the Python helper's default Ollama model.
const DefaultModel = "rnj-1"

func connectCmd(gateway string) tea.Cmd {
	return func() tea.Msg {
		c := NewOllaClient(gateway)
		v, err := c.Version()
		return connectedMsg{gateway: gateway, info: v, err: err}
	}
}

func statusCmd(c *OllaClient) tea.Cmd {
	return func() tea.Msg {
		st, err := c.Status()
		return statusMsg{st: st, err: err}
	}
}

func modelsCmd(c *OllaClient) tea.Cmd {
	return func() tea.Msg {
		ms, err := c.Models()
		return modelsMsg{models: ms, err: err}
	}
}

// workerModelsCmd builds the model inventory by querying every worker directly
// (the union of their installed models). This reflects pulls/deletes
// immediately, unlike the gateway's tag cache which only refreshes on Olla's
// discovery interval (~5m).
func workerModelsCmd(workers []workerRef) tea.Cmd {
	return func() tea.Msg {
		ms, err := aggregateWorkerModels(workers)
		return modelsMsg{models: ms, err: err}
	}
}

func aggregateWorkerModels(workers []workerRef) ([]Model, error) {
	seen := map[string]Model{}
	var firstErr error
	for _, w := range workers {
		ms, err := ollamaModelList(w.url)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, mm := range ms {
			if _, ok := seen[mm.Name]; !ok {
				seen[mm.Name] = mm
			}
		}
	}
	if len(seen) == 0 && firstErr != nil {
		return nil, firstErr
	}
	out := make([]Model, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func vmsCmd(cfg *PCConfig) tea.Cmd {
	return func() tea.Msg {
		pc := NewPCClient(cfg)
		vms, err := pc.ListVMs()
		clusters, cerr := pc.ClusterNames()
		images, ierr := pc.ImageNames()
		subnets, serr := pc.SubnetNames()
		imageByID, _ := pc.ImagesByID()
		return vmsMsg{
			vms: vms, clusters: clusters, images: images, subnets: subnets,
			imageByID:    imageByID,
			err:          err,
			placementErr: firstErr(cerr, ierr, serr),
		}
	}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// streaming readers -----------------------------------------------------------

func waitChat(ch chan ChatEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return chatEvMsg{Kind: "done"}
		}
		return chatEvMsg(ev)
	}
}

func waitPull(ch chan PullEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return pullEvMsg{Done: true}
		}
		return pullEvMsg(ev)
	}
}

func waitProc(ch chan ProcEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return procEvMsg{Done: true}
		}
		return procEvMsg(ev)
	}
}

func nextNameCmd(cfg *PCConfig, role string) tea.Cmd {
	return func() tea.Msg {
		name, err := NextName(cfg, role)
		return nextNameMsg{name: name, err: err}
	}
}

func sshAddCmd(host, user, pass string, e endpointEntry) tea.Cmd {
	return func() tea.Msg {
		msg, err := ApplyEndpointChange(host, user, pass, addEndpointFn(e))
		return sshResultMsg{msg: msg, err: err}
	}
}

func sshRemoveCmd(host, user, pass, name string) tea.Cmd {
	return func() tea.Msg {
		msg, err := ApplyEndpointChange(host, user, pass, removeEndpointFn(name))
		return sshResultMsg{msg: msg, err: err}
	}
}

// sshAddAndKeyCmd adds the endpoint to the gateway, then best-effort installs the
// managed SSH key on the new worker so it can be consoled into without a
// password (feature: "when a worker registers, create ssh key based auth").
func sshAddAndKeyCmd(host, user, pass string, e endpointEntry) tea.Cmd {
	return func() tea.Msg {
		msg, err := ApplyEndpointChange(host, user, pass, addEndpointFn(e))
		if err != nil {
			return sshResultMsg{msg: msg, err: err}
		}
		if wh := hostFromURL(e.URL); wh != "" {
			if _, kerr := EnsureKeyAuth(wh, user, pass); kerr == nil {
				msg += " · ssh key installed on " + wh
			} else {
				msg += " · key install skipped (" + kerr.Error() + ")"
			}
		}
		return sshResultMsg{msg: msg, err: nil}
	}
}

// prepConsoleCmd ensures the managed SSH key is installed on the target host
// (using the password to bootstrap, idempotent) and returns a consoleReadyMsg so
// the subsequent interactive session can log in without a password.
func prepConsoleCmd(user, host, pass string) tea.Cmd {
	return func() tea.Msg {
		u := orDefault(user, "rocky")
		if host == "" {
			return consoleReadyMsg{user: u, host: host, err: fmt.Errorf("no host")}
		}
		var err error
		if pass != "" {
			_, err = EnsureKeyAuth(host, u, pass)
		}
		return consoleReadyMsg{user: u, host: host, key: managedKeyPath(), err: err}
	}
}

// prepAgentCmd ensures key auth on the target host (bootstrapping with the
// password if needed) and returns a consoleReadyMsg carrying the remote command
// to launch — used by the Agents section to open/deploy an agent CLI over SSH.
func prepAgentCmd(user, host, pass, remoteCmd, label string) tea.Cmd {
	return func() tea.Msg {
		u := orDefault(user, "rocky")
		if host == "" {
			return consoleReadyMsg{user: u, host: host, cmd: remoteCmd, label: label, err: fmt.Errorf("no host")}
		}
		var err error
		if pass != "" {
			_, err = EnsureKeyAuth(host, u, pass)
		}
		return consoleReadyMsg{user: u, host: host, key: managedKeyPath(), cmd: remoteCmd, label: label, err: err}
	}
}

// deployAgentCmd stages an agent's bootstrap script on the target host, then
// returns a consoleReadyMsg that runs it interactively in a login shell (so
// sudo/onboarding prompts work and the full PATH is present). On clean exit the
// agent is registered against host.
func deployAgentCmd(user, host, pass, script, agent, label string) tea.Cmd {
	return func() tea.Msg {
		u := orDefault(user, "rocky")
		if host == "" {
			return notifyMsg("no target host for " + label)
		}
		if pass != "" {
			_, _ = EnsureKeyAuth(host, u, pass)
		}
		const remotePath = "~/.oilsand-deploy.sh"
		if err := uploadRemoteScript(host, u, pass, remotePath, script); err != nil {
			return notifyMsg(label + " prep failed: " + err.Error())
		}
		return consoleReadyMsg{
			user: u, host: host, key: managedKeyPath(),
			cmd: "bash -l " + remotePath, label: label, agent: agent,
		}
	}
}

// crushCmd writes the Olla provider config to the gateway, then either deploys
// (uploads + runs the install script) or opens crush. When script != "" it is a
// deploy (registers the agent on clean exit); otherwise it just launches crush.
func crushCmd(user, host, pass, config, script, label, agent string) tea.Cmd {
	return func() tea.Msg {
		u := orDefault(user, "rocky")
		if host == "" {
			return notifyMsg("connect to a gateway first")
		}
		if pass != "" {
			_, _ = EnsureKeyAuth(host, u, pass)
		}
		// Merge (don't overwrite) so manually-registered MCP servers and other
		// user config in crush.json survive a Crush open/redeploy from the TUI.
		if err := sshMergeCrushConfig(host, u, pass, config); err != nil {
			return notifyMsg("crush config write failed: " + err.Error())
		}
		cmd := loginShell("crush")
		if script != "" {
			const remotePath = "~/.oilsand-deploy.sh"
			if err := uploadRemoteScript(host, u, pass, remotePath, script); err != nil {
				return notifyMsg(label + " prep failed: " + err.Error())
			}
			cmd = "bash -l " + remotePath
		}
		return consoleReadyMsg{
			user: u, host: host, key: managedKeyPath(),
			cmd: cmd, label: label, agent: agent,
		}
	}
}

// refreshGatewayCmd restarts Olla so newly downloaded models are discovered and
// routable right away (Olla re-scans endpoint models on startup). Used after a
// download since Olla v0.0.28 has no discovery-refresh API.
func refreshGatewayCmd(host, user, pass string) tea.Cmd {
	return func() tea.Msg {
		if host == "" || pass == "" {
			return notifyMsg("downloaded — gateway will route the new model within ~5m (no SSH creds to refresh now)")
		}
		if err := RestartOlla(host, orDefault(user, "rocky"), pass); err != nil {
			return notifyMsg("download done, but gateway refresh failed: " + err.Error())
		}
		return notifyMsg("gateway refreshed — new models are ready to use")
	}
}

// keyInstallCmd installs the managed SSH key on a single host (gateway/worker).
func keyInstallCmd(host, user, pass string) tea.Cmd {
	return func() tea.Msg {
		if _, err := EnsureKeyAuth(host, user, pass); err != nil {
			return notifyMsg("key install failed on " + host + ": " + err.Error())
		}
		return notifyMsg("ssh key installed on " + host)
	}
}

// endpointsCmd reads the gateway's configured endpoints over SSH (the status API
// doesn't expose worker URLs, which we need for direct ops and consoles).
func endpointsCmd(host, user, pass string) tea.Cmd {
	return func() tea.Msg {
		eps, err := ReadEndpoints(host, user, pass)
		return endpointsMsg{eps: eps, err: err}
	}
}

// deleteModelAllCmd removes a model from every worker directly (the gateway proxy
// would only hit one). Returns a summary notice.
func deleteModelAllCmd(workers []workerRef, model string) tea.Cmd {
	return func() tea.Msg {
		if len(workers) == 0 {
			return notifyMsg("no workers known — refresh the Pool first")
		}
		var ok, fail int
		var msgs []string
		for _, w := range workers {
			if err := ollamaDelete(w.url, model); err != nil {
				fail++
				msgs = append(msgs, w.name+": "+err.Error())
			} else {
				ok++
			}
		}
		s := fmt.Sprintf("deleted %s on %d/%d workers", model, ok, len(workers))
		if fail > 0 {
			s += " — " + strings.Join(msgs, "; ")
		}
		return notifyMsg(s)
	}
}

// ---- helpers ---------------------------------------------------------------

func normalizeGateway(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	tail := strings.SplitN(raw, "://", 2)[1]
	hostPart := strings.SplitN(tail, "/", 2)[0]
	if !strings.Contains(hostPart, ":") {
		raw = raw + ":40114"
	}
	return raw
}

func hostFromURL(u string) string {
	tail := u
	if i := strings.Index(tail, "://"); i >= 0 {
		tail = tail[i+3:]
	}
	tail = strings.SplitN(tail, "/", 2)[0]
	return strings.SplitN(tail, ":", 2)[0]
}

// tuiSettings is the small persisted state in ~/.oilsand-ai-gateway/tui.json.
type tuiSettings struct {
	Token         string            `json:"olla_token"`
	DefaultModel  string            `json:"default_model"`
	Gateway       string            `json:"gateway"`      // Olla gateway URL (set on first launch)
	SSHUser       string            `json:"ssh_user"`     // SSH user for the gateway VM
	SSHPass       string            `json:"ssh_password"` // SSH password for the gateway VM
	Deploy        deploySettings    `json:"deploy"`
	PC            pcOverride        `json:"prism_central"`
	Agents        map[string]string   `json:"agents"`      // agent name -> most recent deployed host
	AgentHosts    map[string][]string `json:"agent_hosts"` // agent name -> every host it was deployed on
	Hermes        hermesSettings    `json:"hermes"`
	CustomDeploys []customDeploy    `json:"custom_deploys"` // user-defined deployment types
	CustomSeeded  bool              `json:"custom_seeded"`  // built-in custom deploys seeded once (so deletes stick)
	VMImages      map[string]string `json:"vm_images"`      // VM name -> source image name
}

// customDeploy is a user-defined Nutanix deployment type: a friendly name plus
// the URL of a setup script run on the new VM (curl | sudo bash) after the image
// boots. Deploying one provisions a VM from the configured image, then runs the
// script.
type customDeploy struct {
	Name      string `json:"name"`
	ScriptURL string `json:"script_url"`
	// Optional access link for the deployed workload, shown (clickable) after a
	// successful deploy as <scheme>://<vm-ip>:<port><path>.
	Scheme string `json:"scheme,omitempty"`
	Port   string `json:"port,omitempty"`
	Path   string `json:"path,omitempty"`
}

// hermesSettings holds the one-time inputs that let Hermes deploys set up the
// messaging gateway (Telegram) with near-zero interaction. The bot token is the
// single unavoidable secret (from BotFather); allowed users authorize accounts.
type hermesSettings struct {
	TelegramBotToken     string `json:"telegram_bot_token"`
	TelegramAllowedUsers string `json:"telegram_allowed_users"` // comma-separated IDs
	TelegramHomeChannel  string `json:"telegram_home_channel"`  // optional
	GatewayEnabled       bool   `json:"gateway_enabled"`        // auto-setup gateway on deploy
	GatewayMode          string `json:"gateway_mode"`           // "user" | "system"
}

// gatewayMode returns the effective install mode ("user" by default).
func (h hermesSettings) gatewayMode() string {
	if h.GatewayMode == "system" {
		return "system"
	}
	return "user"
}

// deploySettings overrides the Nutanix VM template + image used when deploying
// new gateways/workers. Zero values fall back to the Python helper's defaults.
type deploySettings struct {
	ImageName      string `json:"image_name"`
	ClusterName    string `json:"cluster_name"`
	SubnetName     string `json:"subnet_name"`
	NumSockets     int    `json:"num_sockets"`
	CoresPerSocket int    `json:"cores_per_socket"`
	MemoryGiB      int    `json:"memory_gib"`
	DiskGiB        int    `json:"disk_gib"`
	VMUser         string `json:"vm_user"`
	VMPassword     string `json:"vm_password"`
}

// pcOverride optionally replaces the Prism Central connection read from
// ~/.cursor/mcp.json (instance + service account / API key).
type pcOverride struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	APIKey   string `json:"api_key"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// deployDefaults mirrors the Python helper's DEFAULT_* constants so the settings
// form can pre-fill sensible values.
func deployDefaults() deploySettings {
	return deploySettings{
		// Image / cluster / subnet have no baked-in defaults: they are chosen
		// from the live Prism Central inventory in the Nutanix settings form.
		ImageName:      "",
		ClusterName:    "",
		SubnetName:     "",
		NumSockets:     2,
		CoresPerSocket: 4,
		MemoryGiB:      12,
		DiskGiB:        50,
		VMUser:         "rocky",
		// No default VM password: it is supplied on first launch via the
		// Nutanix settings form (or OILSAND_VM_PASSWORD for the CLI helper).
		VMPassword: "",
	}
}

// legacyPlacement holds the lab-specific placement names that used to be baked
// into the code/state. They are cleared from saved settings so they never
// reappear as phantom selections in a different Prism Central environment.
var legacyPlacement = map[string]bool{
	"canucks":               true,
	"canucks.primary.vlan0": true,
	"Rocky-9-GenericCloud-Base.latest.x86_64.qcow2": true,
}

// dropLegacyPlacement clears any placement value that matches an old hardcoded
// lab default, so deploy forms don't pre-seed a cluster/subnet/image that
// doesn't exist on the user's PC.
func dropLegacyPlacement(d deploySettings) deploySettings {
	if legacyPlacement[d.ClusterName] {
		d.ClusterName = ""
	}
	if legacyPlacement[d.SubnetName] {
		d.SubnetName = ""
	}
	if legacyPlacement[d.ImageName] {
		d.ImageName = ""
	}
	return d
}

// withDeployDefaults fills any zero fields of d from the helper defaults.
func withDeployDefaults(d deploySettings) deploySettings {
	def := deployDefaults()
	d.ImageName = orDefault(d.ImageName, def.ImageName)
	d.ClusterName = orDefault(d.ClusterName, def.ClusterName)
	d.SubnetName = orDefault(d.SubnetName, def.SubnetName)
	d.VMUser = orDefault(d.VMUser, def.VMUser)
	d.VMPassword = orDefault(d.VMPassword, def.VMPassword)
	if d.NumSockets <= 0 {
		d.NumSockets = def.NumSockets
	}
	if d.CoresPerSocket <= 0 {
		d.CoresPerSocket = def.CoresPerSocket
	}
	if d.MemoryGiB <= 0 {
		d.MemoryGiB = def.MemoryGiB
	}
	if d.DiskGiB <= 0 {
		d.DiskGiB = def.DiskGiB
	}
	return d
}

func loadSettings(path string) tuiSettings {
	var s tuiSettings
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func saveSettings(path string, s tuiSettings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(path, raw, 0o600)
}

func loadToken(path string) string { return loadSettings(path).Token }

func saveToken(path, token string) error {
	s := loadSettings(path)
	s.Token = token
	return saveSettings(path, s)
}

// saveConnect persists the gateway connection entered on first launch so later
// runs reconnect without re-prompting. The SSH password is stored in tui.json
// (mode 0600) alongside the other service-account secrets.
func saveConnect(path, gateway, sshUser, sshPass string) error {
	s := loadSettings(path)
	s.Gateway = gateway
	s.SSHUser = sshUser
	s.SSHPass = sshPass
	return saveSettings(path, s)
}

func loadDefaultModel(path string) string { return loadSettings(path).DefaultModel }

func saveDefaultModel(path, model string) error {
	s := loadSettings(path)
	s.DefaultModel = model
	return saveSettings(path, s)
}

// saveAgentReg persists agent->host registrations: the most recent host per
// agent plus the full per-agent host list (multi-worker agents like Nanoclaw
// are updated on every host they were ever deployed to).
func saveAgentReg(path string, reg map[string]string, hosts map[string][]string) error {
	s := loadSettings(path)
	s.Agents = reg
	s.AgentHosts = hosts
	return saveSettings(path, s)
}

// saveHermesCfg persists the Telegram/gateway settings used by Hermes deploys.
func saveHermesCfg(path string, h hermesSettings) error {
	s := loadSettings(path)
	s.Hermes = h
	return saveSettings(path, s)
}

// saveDeployPC persists the deploy template + Prism Central overrides.
func saveDeployPC(path string, d deploySettings, pc pcOverride) error {
	s := loadSettings(path)
	s.Deploy = d
	s.PC = pc
	return saveSettings(path, s)
}

// saveCustomDeploys persists the user-defined custom deployment types and marks
// the built-in defaults as seeded (so deleting them all doesn't re-add them).
func saveCustomDeploys(path string, cds []customDeploy) error {
	s := loadSettings(path)
	s.CustomDeploys = cds
	s.CustomSeeded = true
	return saveSettings(path, s)
}

// saveVMImages persists the VM name -> source image map.
func saveVMImages(path string, m map[string]string) error {
	s := loadSettings(path)
	s.VMImages = m
	return saveSettings(path, s)
}

// pcConfigFromOverride builds a PCConfig from an override if it carries enough
// to connect (host + an API key or user/password). Returns nil otherwise.
func pcConfigFromOverride(o pcOverride) *PCConfig {
	host := strings.TrimSpace(o.Host)
	if host == "" {
		return nil
	}
	if strings.TrimSpace(o.APIKey) == "" && strings.TrimSpace(o.User) == "" {
		return nil
	}
	port := orDefault(strings.TrimSpace(o.Port), "9440")
	return &PCConfig{
		Host:     host,
		Port:     port,
		APIKey:   strings.TrimSpace(o.APIKey),
		User:     strings.TrimSpace(o.User),
		Password: o.Password,
	}
}

// spark renders a sparkline from values using block runes.
func spark(values []float64, width int) string {
	if len(values) == 0 || width <= 0 {
		return ""
	}
	runes := []rune("▁▂▃▄▅▆▇█")
	if len(values) > width {
		values = values[len(values)-width:]
	}
	var max float64
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	for _, v := range values {
		idx := 0
		if max > 0 {
			idx = int(v / max * float64(len(runes)-1))
		}
		if idx < 0 {
			idx = 0
		}
		if idx >= len(runes) {
			idx = len(runes) - 1
		}
		b.WriteRune(runes[idx])
	}
	return b.String()
}
