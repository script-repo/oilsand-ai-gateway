package main

import (
	"strings"
	"testing"
)

// The OmniRoute agent deploys the official image as one service container on a
// worker, exposing the dashboard/API port and persisting to a named volume.
func TestOmnirouteDeployScript(t *testing.T) {
	m := &model{gateway: "http://10.0.0.1:40114", token: "tok", defModel: "rnj-1"}
	script := m.omnirouteDeployScript()
	for _, want := range []string{
		"ensuring Docker",           // reuses the shared docker bootstrap
		"sudo docker pull \"$IMG\"", // pulls the image before running
		"--name omniroute",          // single named service container
		"-p " + omniroutePort + ":" + omniroutePort,
		"-v omniroute-data:/app/data", // persistent SQLite/config volume
		omnirouteImage,
		":" + omniroutePort, // prints the reachable dashboard URL
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("deploy script missing %q:\n%s", want, script)
		}
	}
}

// OmniRoute must be a deployable worker agent in the catalog so it shows up in
// the Agents section and flows through the ordinary host-install path.
func TestOmnirouteInCatalog(t *testing.T) {
	a, ok := agentByName("OmniRoute")
	if !ok {
		t.Fatal("OmniRoute missing from agent catalog")
	}
	if !a.deployable || a.target != "worker" {
		t.Fatalf("OmniRoute should be a deployable worker agent, got %+v", a)
	}
	if a.container {
		t.Fatal("OmniRoute is a single service, not a Nanoclaw-style multi-instance container agent")
	}
}

// Update recreates the container on the freshly pulled image; the data volume is
// re-attached so provider/key config survives the swap.
func TestOmnirouteUpdateScript(t *testing.T) {
	s := omnirouteUpdateScript()
	for _, want := range []string{"docker pull \"$IMG\"", "--name omniroute", "-v omniroute-data:/app/data"} {
		if !strings.Contains(s, want) {
			t.Fatalf("update script missing %q:\n%s", want, s)
		}
	}
}

// Removing OmniRoute deletes the service container but keeps its data volume.
func TestOmnirouteUninstallScript(t *testing.T) {
	a, _ := agentByName("OmniRoute")
	s := agentUninstallScript(a)
	if !strings.Contains(s, "docker rm -f omniroute") {
		t.Fatalf("uninstall script should remove the container:\n%s", s)
	}
	if strings.Contains(s, "volume rm") {
		t.Fatalf("uninstall should keep the data volume:\n%s", s)
	}
}

// Registering OmniRoute prefills the add-endpoint modal for its own OpenAI
// endpoint (type "openai", port omniroutePort) — never as an Ollama worker.
func TestOpenOmnirouteEndpointPrefill(t *testing.T) {
	m := &model{
		gateway:  "http://10.0.0.1:40114",
		agentReg: map[string]string{"OmniRoute": "10.0.0.9"},
		contentW: 60,
	}
	if cmd := m.openOmnirouteEndpoint(); cmd == nil {
		t.Fatal("openOmnirouteEndpoint returned no command (form did not open)")
	}
	if m.modal != modalEndpoint {
		t.Fatalf("expected modalEndpoint, got %v", m.modal)
	}
	if m.fEpName != "omniroute" {
		t.Fatalf("name = %q, want omniroute", m.fEpName)
	}
	if m.fEpType != "openai" {
		t.Fatalf("type = %q, want openai", m.fEpType)
	}
	if !strings.Contains(m.fEpURL, "10.0.0.9") || !strings.Contains(m.fEpURL, ":"+omniroutePort) {
		t.Fatalf("url = %q, want host 10.0.0.9 and port %s", m.fEpURL, omniroutePort)
	}
}

// Registering OmniRoute must APPEND a new, distinct endpoint — it must not merge
// into or disturb the existing Ollama worker endpoints.
func TestOmnirouteEndpointIsSeparate(t *testing.T) {
	base := `discovery:
  static:
    endpoints:
      - url: "http://10.0.0.4:11434"
        name: "ollama-worker-04"
        type: "ollama"
        priority: 100
      - url: "http://10.0.0.5:11434"
        name: "ollama-worker-05"
        type: "ollama"
        priority: 100
`
	e := endpointEntry{Name: "omniroute", URL: "http://10.0.0.9:" + omniroutePort, Type: "openai", Priority: 100}
	out, err := mutateEndpoints(base, addEndpointFn(e))
	if err != nil {
		t.Fatalf("mutateEndpoints: %v", err)
	}
	eps := readEndpointsFromYAML(out)
	if len(eps) != 3 {
		t.Fatalf("expected 3 endpoints (2 workers + omniroute), got %d: %+v", len(eps), eps)
	}
	// Both workers survive untouched, and there's exactly one omniroute openai entry.
	var workers, omni int
	for _, ep := range eps {
		switch {
		case ep.Type == "ollama" && strings.HasPrefix(ep.Name, "ollama-worker-"):
			workers++
			if !strings.Contains(ep.URL, ":11434") {
				t.Fatalf("worker %s url changed: %q", ep.Name, ep.URL)
			}
		case ep.Name == "omniroute":
			omni++
			if ep.Type != "openai" || !strings.Contains(ep.URL, ":"+omniroutePort) {
				t.Fatalf("omniroute entry wrong: %+v", ep)
			}
		default:
			t.Fatalf("unexpected endpoint: %+v", ep)
		}
	}
	if workers != 2 || omni != 1 {
		t.Fatalf("want 2 workers + 1 omniroute, got %d workers, %d omniroute", workers, omni)
	}
}

// De-registering OmniRoute removes only its own endpoint(s), leaving the Ollama
// workers in place.
func TestRemoveOmnirouteEndpointsFn(t *testing.T) {
	eps := []endpointEntry{
		{Name: "ollama-worker-04", URL: "http://10.0.0.4:11434", Type: "ollama"},
		{Name: "omniroute", URL: "http://10.0.0.9:" + omniroutePort, Type: "openai"},
		{Name: "ollama-worker-05", URL: "http://10.0.0.5:11434", Type: "ollama"},
	}
	out := removeOmnirouteEndpointsFn([]string{"10.0.0.9"})(eps)
	if len(out) != 2 {
		t.Fatalf("expected 2 endpoints after removing omniroute, got %d: %+v", len(out), out)
	}
	for _, ep := range out {
		if ep.Name == "omniroute" || ep.Type == "openai" {
			t.Fatalf("omniroute endpoint not removed: %+v", out)
		}
	}
}

// "Open" on a background service follows the container logs rather than trying
// to launch a (non-existent) omniroute CLI on the host.
func TestOmnirouteOpenCmd(t *testing.T) {
	a, _ := agentByName("OmniRoute")
	cmd := agentOpenCmd(a)
	if !strings.Contains(cmd, "docker logs -f") || !strings.Contains(cmd, "omniroute") {
		t.Fatalf("open command should follow omniroute logs:\n%s", cmd)
	}
}
