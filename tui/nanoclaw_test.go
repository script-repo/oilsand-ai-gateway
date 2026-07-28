package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The image ships a Buzz join script; buzz-cli itself is optional (the public
// block/buzz image has buzz-relay/admin only — copying buzz-cli from it used
// to fail the whole Nanoclaw image build).
func TestNanoclawImageShipsBuzzJoin(t *testing.T) {
	for _, want := range []string{
		"/usr/local/bin/oilsand-join-buzz.sh",
		"OILSAND_BUZZ_RELAY_URL",
		"buzz users set-presence",
		"buzz channels join",
	} {
		if !strings.Contains(nanoclawDockerfile, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
	if strings.Contains(nanoclawDockerfile, "FROM ghcr.io/block/buzz:main AS buzzcli") {
		t.Error("multi-stage buzzcli copy breaks the build (image has no /usr/local/bin/buzz)")
	}
	// Join must not block NanoClaw itself from starting.
	if !strings.Contains(nanoclawDockerfile, "oilsand-join-buzz.sh >> /var/log/oilsand-buzz.log 2>&1 &") {
		t.Error("Buzz join is not started in the background")
	}
	// ncl has to be on PATH for the interactive shell to be useful.
	if !strings.Contains(nanoclawDockerfile, "ln -sf /opt/nanoclaw/bin/ncl /usr/local/bin/ncl") {
		t.Error("ncl is not linked onto PATH")
	}
}

// Open must land on the CLI-channel chat loop, not bare ncl or a plain shell.
func TestNanoclawImageShipsCLIChat(t *testing.T) {
	for _, want := range []string{
		"/usr/local/bin/oilsand-nanoclaw-chat.sh",
		"scripts/init-cli-agent.ts",
		"scripts/chat.ts",
		"data/cli.sock",
		"you>",
	} {
		if !strings.Contains(nanoclawDockerfile, want) {
			t.Errorf("Dockerfile missing CLI chat wiring %q", want)
		}
	}
	// data/groups must survive outer-container recreate.
	if !strings.Contains(nanoclawDockerfile, `ln -sfn "/opt/nanoclaw/store/$d" "/opt/nanoclaw/$d"`) {
		t.Error("entrypoint does not persist data/groups on the state volume")
	}
}

// nanoclawDockerfile is a Go raw string literal, so a stray backtick anywhere
// inside it would end the literal and break the build in a confusing way.
// Guard it, since the content is shell and JavaScript where backticks are
// otherwise idiomatic.
func TestNanoclawEmbeddedScriptsAvoidBackticks(t *testing.T) {
	for name, s := range map[string]string{
		"nanoclawDockerfile":  nanoclawDockerfile,
		"nanoclawShellRC":     nanoclawShellRC,
		"nanoclawChatFallback": nanoclawChatFallback,
	} {
		if strings.Contains(s, "`") {
			t.Errorf("%s contains a backtick, which cannot appear in a raw string literal", name)
		}
	}
}

// The image must install OneCLI, write .env, and register an Olla-facing
// secret — trunk NanoClaw refuses agent spawns unless applyContainerConfig
// succeeds, and the Claude provider only redirects when ANTHROPIC_BASE_URL
// is in .env (not merely a Docker -e).
func TestNanoclawImageShipsOllaWiring(t *testing.T) {
	for _, want := range []string{
		"/usr/local/bin/oilsand-configure-olla.sh",
		"src/providers/oilsand-olla.ts",
		"OILSAND_OLLA_ANTHROPIC_URL",
		"onecli secrets create",
		"OilsandOlla",
		"ANTHROPIC_BASE_URL",
		"ONECLI_API_KEY",
		"/olla/anthropic", // migration rewrite target for older deploys
	} {
		if !strings.Contains(nanoclawDockerfile, want) {
			t.Errorf("Dockerfile missing Olla/OneCLI wiring %q", want)
		}
	}
	if !strings.Contains(nanoclawDockerfile, "oilsand-configure-olla.sh >> /var/log/oilsand-olla.log") {
		t.Error("entrypoint does not run oilsand-configure-olla.sh")
	}
}

// Deployed containers need Buzz coordinates, and the per-instance name has to
// be the container name so the Buzz tab can attribute presence.
func TestNanoclawDeployScriptPassesBuzzEnv(t *testing.T) {
	m := newModel("http://10.0.0.1:40114", "rocky", "pw")
	m.tokFile = filepath.Join(t.TempDir(), "tui.json")
	m.buzzChannelID = "chan-uuid-1"

	script := m.nanoclawDeployScript(2)
	for _, want := range []string{
		"-e OILSAND_BUZZ_RELAY_URL='http://10.0.0.1:3000'",
		"-e OILSAND_BUZZ_CHANNEL_ID='chan-uuid-1'",
		"-e OILSAND_BUZZ_CHANNEL='" + buzzChannelName + "'",
		`-e OILSAND_BUZZ_NAME="$NAME"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("deploy script missing %q", want)
		}
	}
}

// Agents speak Anthropic Messages API; the OpenAI-shaped URL must not be used
// as ANTHROPIC_BASE_URL (the SDK would call …/openai/v1/v1/messages).
func TestNanoclawDeployScriptPointsAtAnthropicOlla(t *testing.T) {
	m := newModel("http://10.0.0.1:40114", "rocky", "pw")
	m.token = "test-token"
	m.tokFile = filepath.Join(t.TempDir(), "tui.json")

	script := m.nanoclawDeployScript(1)
	for _, want := range []string{
		"-e OILSAND_OLLA_ANTHROPIC_URL='http://10.0.0.1:40114/olla/anthropic'",
		"-e ANTHROPIC_BASE_URL='http://10.0.0.1:40114/olla/anthropic'",
		"-e OILSAND_OLLA_TOKEN='test-token'",
		"-e OPENAI_BASE_URL='http://10.0.0.1:40114/olla/openai/v1'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("deploy script missing %q", want)
		}
	}
	// Guard the old mistake: Anthropic env must not point at the OpenAI path.
	if strings.Contains(script, "ANTHROPIC_BASE_URL='http://10.0.0.1:40114/olla/openai") {
		t.Error("ANTHROPIC_BASE_URL incorrectly points at /olla/openai")
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
