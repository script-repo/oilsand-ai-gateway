package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// isolateHome points the TUI's config/key directories at a temp dir so tests
// never read or write the developer's real ~/.oilsand-ai-gateway.
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)        // unix
	t.Setenv("USERPROFILE", dir) // windows
	return dir
}

// ollaStub serves the /version endpoint the startup probe looks for.
func ollaStub() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"Olla","version":"0.0.28","edition":"standard"}`))
	})
	return httptest.NewServer(mux)
}

func TestFirstRespondingOllaPrefersEarlierCandidate(t *testing.T) {
	srv := ollaStub()
	defer srv.Close()

	dead := "http://127.0.0.1:1" // nothing listens on port 1

	if got := firstRespondingOlla([]string{dead, srv.URL}, 2*time.Second); got != srv.URL {
		t.Fatalf("expected fallback to the live gateway %q, got %q", srv.URL, got)
	}
	if got := firstRespondingOlla([]string{srv.URL, dead}, 2*time.Second); got != srv.URL {
		t.Fatalf("expected the first live gateway %q, got %q", srv.URL, got)
	}
	if got := firstRespondingOlla([]string{dead}, 2*time.Second); got != "" {
		t.Fatalf("expected no gateway, got %q", got)
	}
}

func TestLocalOllaCandidatesAreUnique(t *testing.T) {
	got := localOllaCandidates()
	if len(got) == 0 {
		t.Fatal("expected at least one candidate URL")
	}
	seen := map[string]bool{}
	for _, u := range got {
		if seen[u] {
			t.Fatalf("duplicate candidate %q in %v", u, got)
		}
		seen[u] = true
		if !strings.HasSuffix(u, ":"+LocalOllaPort) {
			t.Errorf("candidate %q does not target the Olla port %s", u, LocalOllaPort)
		}
	}
}

// With no saved gateway the TUI probes for a local Olla, and the Connect form
// must not open on top of that probe.
func TestStartupProbeDefersConnectForm(t *testing.T) {
	isolateHome(t)

	m := newModel("", "", "")
	if !m.probingLocal {
		t.Fatal("expected a local probe to be in flight when no gateway is configured")
	}

	m = drive(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.form != nil {
		t.Fatal("Connect form opened while the local probe was still in flight")
	}

	// Probe found a gateway: adopt it instead of prompting.
	m = drive(m, localOllaFoundMsg{gateway: "http://10.0.0.9:40114"})
	if m.probingLocal {
		t.Error("probe flag should clear once the probe reports back")
	}
	if m.gateway != "http://10.0.0.9:40114" {
		t.Errorf("gateway not adopted from the probe, got %q", m.gateway)
	}
	if m.form != nil {
		t.Error("Connect form should stay closed when a local gateway was found")
	}
}

// When the probe finds nothing, the Connect form still opens as before.
func TestStartupProbeFallsBackToConnectForm(t *testing.T) {
	isolateHome(t)

	m := newModel("", "", "")
	m = drive(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = drive(m, firstRunMsg{})

	if m.probingLocal {
		t.Error("probe flag should clear on firstRunMsg")
	}
	if m.form == nil {
		t.Fatal("expected the Connect form to open when no local gateway was found")
	}
}

// A configured gateway is used as-is; no local probe should be started.
func TestConfiguredGatewaySkipsProbe(t *testing.T) {
	isolateHome(t)

	m := newModel("http://10.0.0.1:40114", "rocky", "secret")
	if m.probingLocal {
		t.Error("should not probe for a local gateway when one is configured")
	}
}

// Every deploy must authorize the managed key at first boot, so no later SSH
// needs the guest password.
func TestDeployFlagsCarrySSHPublicKey(t *testing.T) {
	isolateHome(t)

	m := newModel("http://10.0.0.1:40114", "rocky", "secret")
	flags := m.deployFlags()

	idx := -1
	for i, f := range flags {
		if f == "--ssh-pubkey" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("--ssh-pubkey missing from deploy flags: %v", flags)
	}
	if idx+1 >= len(flags) {
		t.Fatal("--ssh-pubkey has no value")
	}
	if key := flags[idx+1]; !strings.HasPrefix(key, "ssh-ed25519 ") {
		t.Errorf("expected an ed25519 authorized_keys line, got %q", key)
	}
}
