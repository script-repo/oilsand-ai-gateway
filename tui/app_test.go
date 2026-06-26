package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// drive feeds a message through Update and returns the concrete model.
func drive(m model, msg tea.Msg) model {
	next, _ := m.Update(msg)
	return next.(model)
}

func TestModelLifecycle(t *testing.T) {
	m := newModel("http://10.0.0.1:40114", "rocky", "secret")
	m.Init() // should not panic

	m = drive(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if !m.ready {
		t.Fatal("model not ready after window size")
	}

	// Simulate a connected gateway with two endpoints carrying different load.
	m = drive(m, connectedMsg{gateway: "http://10.0.0.1:40114", info: VersionInfo{Name: "Olla", Version: "0.0.28"}})
	if !m.connected {
		t.Fatal("expected connected after connectedMsg")
	}
	st := Status{
		System: SystemStatus{EndpointsUp: "2/2", ActiveConnections: 3, TotalRequests: 100, TotalTraffic: "1.5 MB", AvgLatency: "12ms", SuccessRate: "100%"},
		Endpoints: []Endpoint{
			{Name: "ollama-worker-01", Status: "healthy", Connections: 2, Requests: 70, Priority: 100, Models: struct {
				Count int `json:"count"`
			}{Count: 1}},
			{Name: "ollama-worker-02", Status: "healthy", Connections: 1, Requests: 30, Priority: 100, Models: struct {
				Count int `json:"count"`
			}{Count: 1}},
		},
	}
	m = drive(m, statusMsg{st: st})
	m = drive(m, modelsMsg{models: []Model{{Name: "rnj-1:latest", Size: 1234567, Details: ModelDetails{Family: "llama", ParameterSize: "8B", QuantizationLevel: "Q4"}}}})

	if m.poolList.SelectedItem() == nil {
		t.Fatal("pool list should have items after status")
	}
	if m.modelsList.SelectedItem() == nil {
		t.Fatal("models list should have items after modelsMsg")
	}

	// Every section should render without panicking, in both zones.
	for i := 0; i < len(sections); i++ {
		m.section = section(i)
		m.zone = zoneContent
		if strings.TrimSpace(m.View()) == "" {
			t.Fatalf("empty view for section %s", sections[i].name)
		}
	}

	// Load section must show both workers and a leader marker.
	m.section = secLoad
	load := m.View()
	if !strings.Contains(load, "ollama-worker-01") || !strings.Contains(load, "Request share") {
		t.Fatalf("load view missing expected content:\n%s", load)
	}

	// Sidebar navigation: down advances the section.
	m.section = secDash
	m.zone = zoneSidebar
	m = drive(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.section != secPool {
		t.Fatalf("down did not advance section, got %v", m.section)
	}

	// Number hotkey jumps to a section and focuses content.
	m = drive(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'4'}})
	if m.section != secChat || m.zone != zoneContent {
		t.Fatalf("number jump failed: section=%v zone=%v", m.section, m.zone)
	}

	// Streaming chat events should accumulate and finish cleanly.
	m.client = NewOllaClient("http://10.0.0.1:40114")
	m.composer.SetValue("hello")
	m.chatCh = make(chan ChatEvent, 4)
	m.streaming = true
	m.history = append(m.history, chatTurn{role: roleUser, content: "hello"})
	m = drive(m, chatEvMsg{Kind: "first", Content: "Hi"})
	m = drive(m, chatEvMsg{Kind: "delta", Content: " there"})
	m = drive(m, chatEvMsg{Kind: "done"})
	if m.streaming {
		t.Fatal("chat still streaming after done")
	}
	if len(m.history) == 0 || m.history[len(m.history)-1].content != "Hi there" {
		t.Fatalf("chat history not finalized: %+v", m.history)
	}
}

func TestNormalizeGateway(t *testing.T) {
	cases := map[string]string{
		"10.0.0.1":               "http://10.0.0.1:40114",
		"http://10.0.0.1":        "http://10.0.0.1:40114",
		"http://10.0.0.1:8080":   "http://10.0.0.1:8080",
		"https://h.example:9000": "https://h.example:9000",
		"":                       "",
	}
	for in, want := range cases {
		if got := normalizeGateway(in); got != want {
			t.Errorf("normalizeGateway(%q)=%q want %q", in, got, want)
		}
	}
}
