package main

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// removeModel builds a connected model with a temp settings file and the given
// Nanoclaw deployment registrations.
func removeModel(t *testing.T, hosts ...string) model {
	t.Helper()
	m := newModel("http://10.0.0.1:40114", "rocky", "pw")
	m.tokFile = filepath.Join(t.TempDir(), "tui.json")
	m.agentReg = map[string]string{}
	m.agentHosts = map[string][]string{}
	if len(hosts) > 0 {
		m.agentReg["Nanoclaw"] = hosts[0]
		m.agentHosts["Nanoclaw"] = append([]string(nil), hosts...)
	}
	m.refreshAgents()
	return drive(m, tea.WindowSizeMsg{Width: 120, Height: 40})
}

// selectAgent moves the agents-list cursor onto the named agent.
func selectAgent(t *testing.T, m *model, name string) {
	t.Helper()
	for i, it := range m.agentsList.Items() {
		if ai, ok := it.(agentItem); ok && ai.name == name {
			m.agentsList.Select(i)
			return
		}
	}
	t.Fatalf("agent %s not in list", name)
}

func TestAgentUninstallScripts(t *testing.T) {
	for name, want := range map[string]string{
		"Crush":    "dnf -y remove crush",
		"OpenClaw": "npm uninstall -g openclaw",
		"Hermes":   "gateway stop",
	} {
		a, ok := agentByName(name)
		if !ok {
			t.Fatalf("%s missing from catalog", name)
		}
		s := agentUninstallScript(a)
		if !strings.Contains(s, want) {
			t.Fatalf("%s uninstall script missing %q:\n%s", name, want, s)
		}
	}
}

func TestNanoclawRemoveScript(t *testing.T) {
	s := nanoclawRemoveScript([]string{"nanoclaw-02"}, true)
	for _, want := range []string{"docker rm -f 'nanoclaw-02'", "docker volume rm 'oilsand-nanoclaw-02'", "docker volume rm 'oilsand-nanoclaw-02-docker'"} {
		if !strings.Contains(s, want) {
			t.Fatalf("remove script missing %q:\n%s", want, s)
		}
	}
	if s := nanoclawRemoveScript([]string{"nanoclaw-02"}, false); strings.Contains(s, "volume rm") {
		t.Fatalf("volume kept but script still removes it:\n%s", s)
	}
}

func TestRemoveNanoclawOpensInstancePicker(t *testing.T) {
	m := removeModel(t, "10.0.0.4", "10.0.0.5")
	m.section = secAgents
	m.zone = zoneContent
	selectAgent(t, &m, "Nanoclaw")

	// x kicks off the instance fetch with the remove intent recorded.
	next, _ := m.handleContentKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}, "x")
	m = next.(model)
	if m.pendingRemove != "Nanoclaw" || !m.nanoInstBusy {
		t.Fatalf("remove did not start instance fetch: pending=%q busy=%v", m.pendingRemove, m.nanoInstBusy)
	}

	// The inventory arriving opens the per-instance picker.
	rows := []nanoclawInstance{
		{host: "10.0.0.4", name: "nanoclaw-01", status: "Up 2 hours", image: nanoclawImage},
		{host: "10.0.0.5", name: "nanoclaw-01", status: "Up 5 minutes", image: nanoclawImage},
	}
	m = drive(m, nanoclawInstancesMsg{rows: rows, hosts: 2, okHosts: []string{"10.0.0.4", "10.0.0.5"}})
	if m.pendingRemove != "" {
		t.Fatal("pendingRemove not cleared")
	}
	if m.form == nil || m.modal != modalAgentRemove {
		t.Fatalf("instance picker not opened: modal=%v", m.modal)
	}

	// Completing the picker (form-nil fallback path) dispatches the removal.
	m.form = nil
	m.fRemoveTarget = "10.0.0.4|nanoclaw-01"
	m.fRemoveVols = true
	if cmd := m.onFormComplete(); cmd == nil {
		t.Fatal("remove picker completion produced no command")
	}
	if !strings.Contains(m.notice, "removing 1 nanoclaw instance") {
		t.Fatalf("unexpected notice: %q", m.notice)
	}
}

func TestNanoclawConnectRemoteCmd(t *testing.T) {
	got := nanoclawConnectRemoteCmd("nanoclaw-03")
	for _, want := range []string{
		"docker exec -it",
		"'nanoclaw-03'",
		"oilsand-nanoclaw-chat.sh", // preferred image launcher
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("connect cmd missing %q: %s", want, got)
		}
	}
	// ncl is a one-shot admin client: never the session command.
	if strings.Contains(got, "tsx src/cli/client.ts") {
		t.Fatalf("connect cmd invokes the one-shot ncl client: %s", got)
	}
	// Fallback chat loop is staged for older images (before the baked launcher).
	if enc := base64.StdEncoding.EncodeToString([]byte(nanoclawChatFallback)); !strings.Contains(got, enc) {
		t.Error("connect cmd does not stage the chat fallback")
	}
	// /shell crib sheet is available via env for the fallback loop.
	if enc := base64.StdEncoding.EncodeToString([]byte(nanoclawShellRC)); !strings.Contains(got, enc) {
		t.Error("connect cmd does not stage the shell rc for /shell")
	}
}

func TestOpenNanoclawOpensConnectPicker(t *testing.T) {
	m := removeModel(t, "10.0.0.4", "10.0.0.5")
	m.section = secAgents
	m.zone = zoneContent
	selectAgent(t, &m, "Nanoclaw")

	// o/enter kicks off the instance fetch with the connect intent recorded
	// (rather than tailing the newest container's logs).
	next, _ := m.handleContentKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}}, "o")
	m = next.(model)
	if !m.pendingConnect || !m.nanoInstBusy {
		t.Fatalf("open did not start instance fetch: pendingConnect=%v busy=%v", m.pendingConnect, m.nanoInstBusy)
	}

	// The inventory arriving opens the per-instance connect picker; only running
	// instances are offered.
	rows := []nanoclawInstance{
		{host: "10.0.0.4", name: "nanoclaw-01", status: "Up 2 hours", image: nanoclawImage},
		{host: "10.0.0.5", name: "nanoclaw-02", status: "Exited (1) 3 minutes ago", image: nanoclawImage},
	}
	m = drive(m, nanoclawInstancesMsg{rows: rows, hosts: 2, okHosts: []string{"10.0.0.4", "10.0.0.5"}})
	if m.pendingConnect {
		t.Fatal("pendingConnect not cleared")
	}
	if m.form == nil || m.modal != modalNanoConnect {
		t.Fatalf("connect picker not opened: modal=%v", m.modal)
	}
	// The stopped container must default off; the running one is preselected.
	if m.fConnectTarget != "10.0.0.4|nanoclaw-01" {
		t.Fatalf("running instance not preselected: %q", m.fConnectTarget)
	}

	// Completing the picker (form-nil fallback path) dispatches the connection.
	m.form = nil
	if cmd := m.onFormComplete(); cmd == nil {
		t.Fatal("connect picker completion produced no command")
	}
	if !strings.Contains(m.notice, "connecting to nanoclaw-01") {
		t.Fatalf("unexpected notice: %q", m.notice)
	}
}

func TestReconcileNanoclawHosts(t *testing.T) {
	m := removeModel(t, "10.0.0.4", "10.0.0.5")

	// Both hosts answered; only .5 still has containers → .4 is dropped.
	m = drive(m, nanoclawInstancesMsg{
		rows:    []nanoclawInstance{{host: "10.0.0.5", name: "nanoclaw-01", status: "Up"}},
		hosts:   2,
		okHosts: []string{"10.0.0.4", "10.0.0.5"},
	})
	if got := m.agentHosts["Nanoclaw"]; len(got) != 1 || got[0] != "10.0.0.5" {
		t.Fatalf("host list not reconciled: %v", got)
	}
	if m.agentReg["Nanoclaw"] != "10.0.0.5" {
		t.Fatalf("registration host not updated: %q", m.agentReg["Nanoclaw"])
	}

	// An unreachable host must NOT be dropped (its state is unknown).
	m = drive(m, nanoclawInstancesMsg{hosts: 1, errs: []string{"10.0.0.5: ssh failed"}})
	if got := m.agentHosts["Nanoclaw"]; len(got) != 1 {
		t.Fatalf("unreachable host was dropped: %v", got)
	}

	// All hosts answered with nothing left → registration fully cleared.
	m = drive(m, nanoclawInstancesMsg{hosts: 1, okHosts: []string{"10.0.0.5"}})
	if _, ok := m.agentReg["Nanoclaw"]; ok {
		t.Fatal("registration not cleared after last instance vanished")
	}
	if _, ok := m.agentHosts["Nanoclaw"]; ok {
		t.Fatal("host list not cleared after last instance vanished")
	}
}

func TestHostAgentRemoveUpdatesRegistry(t *testing.T) {
	m := removeModel(t)
	m.agentReg["Hermes"] = "10.0.0.4"
	m.agentHosts["Hermes"] = []string{"10.0.0.4", "10.0.0.5"}
	m.refreshAgents()

	// x on a multi-host host agent opens the host picker directly.
	m.section = secAgents
	m.zone = zoneContent
	selectAgent(t, &m, "Hermes")
	next, _ := m.handleContentKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}, "x")
	m = next.(model)
	if m.form == nil || m.modal != modalAgentRemove || m.pendingAgent != "Hermes" {
		t.Fatalf("host picker not opened: modal=%v pending=%q", m.modal, m.pendingAgent)
	}
	m.form = nil
	m.modal = modalNone

	// A successful uninstall on one host keeps the other registered.
	m = drive(m, agentRemovedMsg{agent: "Hermes", removed: 1, okHosts: []string{"10.0.0.4"}})
	if got := m.agentHosts["Hermes"]; len(got) != 1 || got[0] != "10.0.0.5" {
		t.Fatalf("host not pruned: %v", got)
	}
	if m.agentReg["Hermes"] != "10.0.0.5" {
		t.Fatalf("registration not moved to remaining host: %q", m.agentReg["Hermes"])
	}

	// Removing the last host clears the registration entirely.
	m = drive(m, agentRemovedMsg{agent: "Hermes", removed: 1, okHosts: []string{"10.0.0.5"}})
	if _, ok := m.agentReg["Hermes"]; ok {
		t.Fatal("registration not cleared after last host removed")
	}

	// A failed uninstall keeps the registry untouched and reports the error.
	m.agentReg["Crush"] = "10.0.0.1"
	m.agentHosts["Crush"] = []string{"10.0.0.1"}
	m = drive(m, agentRemovedMsg{agent: "Crush", errs: []string{"10.0.0.1: ssh failed"}})
	if _, ok := m.agentReg["Crush"]; !ok {
		t.Fatal("failed uninstall cleared the registration")
	}
	if !strings.Contains(m.notice, "failed") {
		t.Fatalf("failure not surfaced in notice: %q", m.notice)
	}
}

func TestRemoveUnregisteredAgentIsNoop(t *testing.T) {
	m := removeModel(t)
	m.section = secAgents
	m.zone = zoneContent
	selectAgent(t, &m, "OpenClaw")
	next, _ := m.handleContentKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}, "x")
	m = next.(model)
	if m.form != nil || m.pendingRemove != "" {
		t.Fatal("remove started for an undeployed agent")
	}
	if !strings.Contains(m.notice, "not registered") {
		t.Fatalf("unexpected notice: %q", m.notice)
	}
}
