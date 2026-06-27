package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// runCmd executes a tea.Cmd but abandons it if it doesn't return promptly, so
// timer commands (cursor blink, ticks) that block for their interval are
// effectively dropped. Returns nil on timeout/nil.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case m := <-ch:
		return m
	case <-time.After(40 * time.Millisecond):
		return nil
	}
}

// pump feeds msg into the model and processes the (instant) follow-up commands
// huh emits for navigation/submit until the modal form closes or a cap is hit.
// It stops as soon as the form is nil, so the post-completion command
// (connect/vms — which would hit the network) is never executed. Blink/tick
// timer commands are dropped via runCmd's timeout.
func pump(t *testing.T, start tea.Model, msg tea.Msg) tea.Model {
	t.Helper()
	mdl := start
	queue := []tea.Msg{msg}
	for i := 0; i < 400 && len(queue) > 0; i++ {
		cur := queue[0]
		queue = queue[1:]
		var cmd tea.Cmd
		mdl, cmd = mdl.Update(cur)
		if mdl.(model).form == nil {
			return mdl
		}
		switch v := runCmd(cmd).(type) {
		case tea.BatchMsg:
			for _, c := range v {
				if mm := runCmd(c); mm != nil {
					queue = append(queue, mm)
				}
			}
		case nil:
		default:
			queue = append(queue, v)
		}
	}
	return mdl
}

// TestConnectFormPersistsTypedInput drives the Connect form through the real
// Update loop (which value-copies the model each cycle) and confirms the typed
// gateway is captured by onFormComplete — i.e. the keyed form read survives the
// copying that silently dropped Value(&field)-bound edits before.
func TestConnectFormPersistsTypedInput(t *testing.T) {
	m := newModel("", "", "")
	m.tokFile = filepath.Join(t.TempDir(), "tui.json")
	m.applyLayout(120, 40)

	initCmd := m.openConnect()
	var mdl tea.Model = m
	if initCmd != nil {
		if msg := initCmd(); msg != nil {
			mdl = pump(t, mdl, msg)
		}
	}
	for _, r := range "9.9.9.9" {
		mdl = pump(t, mdl, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		if mdl.(model).form == nil {
			t.Fatal("form closed before submit")
		}
	}
	for i := 0; i < 4 && mdl.(model).form != nil; i++ {
		mdl = pump(t, mdl, tea.KeyMsg{Type: tea.KeyEnter})
	}
	if got := mdl.(model).gateway; !strings.Contains(got, "9.9.9.9") {
		t.Fatalf("typed gateway not captured by onFormComplete; got %q", got)
	}
}

// TestNutanixFormPersistsPCHost confirms the PC host typed into the Nutanix
// settings form is saved (this is the form the user reported saving blank).
func TestNutanixFormPersistsPCHost(t *testing.T) {
	m := newModel("", "", "")
	m.tokFile = filepath.Join(t.TempDir(), "tui.json")
	m.applyLayout(120, 40)

	initCmd := m.openNutanixCfg()
	var mdl tea.Model = m
	if initCmd != nil {
		if msg := runCmd(initCmd); msg != nil {
			mdl = pump(t, mdl, msg)
		}
	}
	for _, r := range "1.2.3.4" {
		mdl = pump(t, mdl, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	// Walk through every field/group and submit.
	for i := 0; i < 30 && mdl.(model).form != nil; i++ {
		mdl = pump(t, mdl, tea.KeyMsg{Type: tea.KeyEnter})
	}
	if mdl.(model).form != nil {
		t.Fatal("form did not complete")
	}
	// The host may be prefixed by a config the machine already has; what matters
	// is that the typed text reached the saved override (before the fix it was
	// dropped entirely by the model copy).
	if got := mdl.(model).pcOver.Host; !strings.HasSuffix(got, "1.2.3.4") {
		t.Fatalf("typed PC host not saved; pcOver.Host=%q", got)
	}
}
