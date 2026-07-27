package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The image must carry the hub bridge: upstream NanoClaw does not speak the
// Oilsand hub protocol, so without a process of ours inside the container no
// instance ever appears on the channel, however the env is set.
func TestNanoclawImageShipsHubBridge(t *testing.T) {
	for _, want := range []string{
		"/usr/local/bin/oilsand-hub-bridge.cjs", // bridge is baked into the image
		"OILSAND_HUB_HOST",                      // entrypoint reads the hub coordinates
		"type: 'hello'",                         // and speaks the hub's join frame
	} {
		if !strings.Contains(nanoclawDockerfile, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
	// The bridge must not block NanoClaw itself from starting.
	if !strings.Contains(nanoclawDockerfile, "oilsand-hub-bridge.cjs >> /var/log/oilsand-hub-bridge.log 2>&1 &") {
		t.Error("hub bridge is not started in the background")
	}
	// ncl has to be on PATH for the interactive shell to be useful.
	if !strings.Contains(nanoclawDockerfile, "ln -sf /opt/nanoclaw/bin/ncl /usr/local/bin/ncl") {
		t.Error("ncl is not linked onto PATH")
	}
}

// nanoclawDockerfile is a Go raw string literal, so a stray backtick anywhere
// inside it would end the literal and break the build in a confusing way.
// Guard it, since the content is shell and JavaScript where backticks are
// otherwise idiomatic.
func TestNanoclawEmbeddedScriptsAvoidBackticks(t *testing.T) {
	for name, s := range map[string]string{
		"nanoclawDockerfile": nanoclawDockerfile,
		"nanoclawShellRC":    nanoclawShellRC,
	} {
		if strings.Contains(s, "`") {
			t.Errorf("%s contains a backtick, which cannot appear in a raw string literal", name)
		}
	}
}

// Deployed containers need the hub coordinates, and the per-instance name has
// to be the container name so the Hub tab lists something recognizable.
func TestNanoclawDeployScriptPassesHubEnv(t *testing.T) {
	m := newModel("http://10.0.0.1:40114", "rocky", "pw")
	m.tokFile = filepath.Join(t.TempDir(), "tui.json")

	script := m.nanoclawDeployScript(2)
	for _, want := range []string{
		"-e OILSAND_HUB_HOST='10.0.0.1'",
		"-e OILSAND_HUB_PORT='" + hubPort + "'",
		`-e OILSAND_HUB_NAME="$NAME"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("deploy script missing %q", want)
		}
	}
}

func TestNanoclawProbeScripts(t *testing.T) {
	for _, got := range []string{nanoclawStatusScript("nanoclaw-01"), nanoclawLogsScript("nanoclaw-01")} {
		if !strings.Contains(got, "'nanoclaw-01'") {
			t.Errorf("script does not target the container: %s", got)
		}
		// A missing container must yield empty output rather than a failing
		// command, so preflight can tell "absent" from "unreachable host".
		if !strings.Contains(got, "|| true") {
			t.Errorf("script should not fail when the container is absent: %s", got)
		}
		// sudo must never wait for a password: the TUI has no way to answer a
		// prompt here, and a blocking probe hangs the whole UI.
		if !strings.Contains(got, "sudo -n ") {
			t.Errorf("script can block on a sudo password prompt: %s", got)
		}
	}
}

// A container that is not running must produce a notice naming the cause,
// instead of handing the terminal to a docker exec that dies instantly.
func TestNanoclawStatusError(t *testing.T) {
	if err := nanoclawStatusError("nanoclaw-01", "10.0.0.4", "running", ""); err != nil {
		t.Fatalf("a running container should pass preflight, got: %v", err)
	}

	err := nanoclawStatusError("nanoclaw-01", "10.0.0.4", "", "")
	if err == nil || !strings.Contains(err.Error(), "no container nanoclaw-01") {
		t.Fatalf("absent container should be reported as missing, got: %v", err)
	}

	// A crash-looped container should surface its own last log line, which is
	// what actually explains the failure.
	err = nanoclawStatusError("nanoclaw-01", "10.0.0.4", "exited",
		"some earlier line\nError: upgrade tripwire refused to start\n")
	if err == nil {
		t.Fatal("expected an error for an exited container")
	}
	for _, want := range []string{"nanoclaw-01", "exited", "10.0.0.4", "upgrade tripwire"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}

	// With no logs to quote the message still has to say what happened.
	err = nanoclawStatusError("nanoclaw-01", "", "created", "")
	if err == nil || !strings.Contains(err.Error(), "this host") {
		t.Fatalf("expected a local-host message, got: %v", err)
	}
}
