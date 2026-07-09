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

// "Open" on a background service follows the container logs rather than trying
// to launch a (non-existent) omniroute CLI on the host.
func TestOmnirouteOpenCmd(t *testing.T) {
	a, _ := agentByName("OmniRoute")
	cmd := agentOpenCmd(a)
	if !strings.Contains(cmd, "docker logs -f") || !strings.Contains(cmd, "omniroute") {
		t.Fatalf("open command should follow omniroute logs:\n%s", cmd)
	}
}
