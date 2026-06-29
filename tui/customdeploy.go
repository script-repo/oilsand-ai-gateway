package main

import (
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

// customItem is a row in the Nutanix "custom deployments" submenu: either the
// special "add deployment" action (add=true) or one saved deployment type.
type customItem struct {
	name   string
	url    string
	scheme string
	port   string
	path   string
	add    bool
}

func (i customItem) cfg() customDeploy {
	return customDeploy{Name: i.name, ScriptURL: i.url, Scheme: i.scheme, Port: i.port, Path: i.path}
}

func (i customItem) Title() string {
	if i.add {
		return "+ add deployment"
	}
	title := i.name
	if i.port != "" {
		title += "  :" + i.port
	}
	if i.url != "" {
		title += "  →  " + i.url
	}
	return title
}

func (i customItem) Description() string {
	if i.add {
		return "define a new deployment type"
	}
	return i.url
}

func (i customItem) FilterValue() string { return i.Title() }

// accessURL builds the workload link for a deployed VM IP, or "" if no port.
func (c customDeploy) accessURL(ip string) string {
	if c.Port == "" || ip == "" {
		return ""
	}
	scheme := c.Scheme
	if scheme == "" {
		scheme = "http"
	}
	path := c.Path
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return scheme + "://" + ip + ":" + c.Port + path
}

// defaultCustomDeploys are the built-in deployment types seeded on first run.
func defaultCustomDeploys() []customDeploy {
	return []customDeploy{
		{Name: "NP4M", ScriptURL: "curl -fsSL https://raw.githubusercontent.com/script-repo/ntnx-np4m/main/install.sh | sudo bash"},
		{Name: "NRCC", ScriptURL: "curl -fsSL https://raw.githubusercontent.com/script-repo/ntnx-console-client/main/install.sh | bash"},
	}
}

// refreshCustomList rebuilds the submenu: the "add deployment" row first, then
// one row per saved custom deployment type.
func (m *model) refreshCustomList() {
	items := []list.Item{customItem{add: true}}
	for _, c := range m.customDeploys {
		items = append(items, customItem{
			name: c.Name, url: c.ScriptURL,
			scheme: c.Scheme, port: c.Port, path: c.Path,
		})
	}
	m.customList.SetItems(items)
}

// slugifyName turns a friendly deployment name into a DNS-ish VM-name prefix
// (lowercase alphanumerics, single dashes), e.g. "Postgres Node" -> "postgres-node".
func slugifyName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "custom"
	}
	return out
}

// deploySelectedCustom acts on the highlighted submenu row: the "add" row opens
// the new-deployment form; a saved row provisions a VM from the configured image
// and runs its setup script. Names auto-increment from the deployment slug.
func (m *model) deploySelectedCustom() tea.Cmd {
	it, ok := m.customList.SelectedItem().(customItem)
	if !ok {
		return nil
	}
	if it.add {
		return m.openCustomDeploy()
	}
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
	args := []string{"pattern-custom", "--script-url", it.url, "--name-prefix", slugifyName(it.name) + "-"}
	args = append(args, m.deployFlags()...)
	// Remember the workload's access config so we can show a clickable link once
	// the VM's IP is reported.
	cfg := it.cfg()
	m.pendingCustom = &cfg
	m.notice = "deploying " + it.name + " — provisioning VM, then running setup script (see Output)"
	return m.startProc(args, "deploy "+it.name)
}

// deleteSelectedCustom removes the highlighted saved deployment type (the "add"
// row is ignored) and persists the change.
func (m *model) deleteSelectedCustom() {
	it, ok := m.customList.SelectedItem().(customItem)
	if !ok || it.add {
		return
	}
	var out []customDeploy
	for _, c := range m.customDeploys {
		if c.Name == it.name && c.ScriptURL == it.url {
			continue
		}
		out = append(out, c)
	}
	m.customDeploys = out
	_ = saveCustomDeploys(m.tokFile, m.customDeploys)
	m.refreshCustomList()
	m.notice = "deleted custom deployment: " + it.name
}
