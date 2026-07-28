package main

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuzzDeployScript(t *testing.T) {
	s := buzzDeployScript("10.0.0.1")
	for _, want := range []string{
		"/opt/oilsand/buzz",
		"ghcr.io/block/buzz:main",
		"BUZZ_REQUIRE_AUTH_TOKEN=false",
		"BUZZ_AUTO_MIGRATE=true",
		"./run.sh start",
		"_liveness",
		"OILSAND_BUZZ_OPERATOR_KEY",
		"oilsand", // default channel name
		"ensuring Docker",
		"ensuring git, openssl, curl", // bare gateway images lack these
		"git clone",
		"ERROR: git still missing",
		"buzz_run", // host or docker fallback for channel create
	} {
		if !strings.Contains(s, want) {
			t.Errorf("deploy script missing %q", want)
		}
	}
}

func TestBuzzSectionWiring(t *testing.T) {
	if sections[secBuzz].name != "Buzz" {
		t.Fatalf("secBuzz is %q", sections[secBuzz].name)
	}
	m := newModel("http://10.0.0.1:40114", "rocky", "pw")
	m.tokFile = filepath.Join(t.TempDir(), "tui.json")
	m.section = secBuzz
	m.zone = zoneContent
	if m.buzzRelayHTTP() != "http://10.0.0.1:3000" {
		t.Fatalf("relay URL: %q", m.buzzRelayHTTP())
	}
}

func TestParseBuzzPoll(t *testing.T) {
	payload := `[{"author_name":"nanoclaw-01","content":"hello","created_at":"2026-01-01T12:00:00Z"}]`
	out := "CHAN abc-def\nRC 0\nB64 " + b64s(payload) + "\nERRB64 \n"
	msg := parseBuzzPoll(1, out)
	if msg.err != nil {
		t.Fatalf("err=%v", msg.err)
	}
	if msg.channel != "abc-def" {
		t.Fatalf("channel=%q", msg.channel)
	}
	if len(msg.lines) != 1 || msg.lines[0].text != "hello" {
		t.Fatalf("lines=%+v", msg.lines)
	}
}

func TestParseBuzzPollEmptyIsOK(t *testing.T) {
	out := "CHAN uuid-here\nRC 0\nB64 " + b64s("[]") + "\nERRB64 \n"
	msg := parseBuzzPoll(2, out)
	if msg.err != nil {
		t.Fatalf("empty feed should not error: %v", msg.err)
	}
	if msg.channel != "uuid-here" || len(msg.lines) != 0 {
		t.Fatalf("msg=%+v", msg)
	}
}

func TestParseBuzzPollExtractsFromNoise(t *testing.T) {
	// Legacy poll without base64 framing.
	out := "Welcome to Rocky\nCHAN ch1\n" + `[{"pubkey":"aabbccdd","content":"hi","created_at":1}]` + "\n"
	msg := parseBuzzPoll(3, out)
	if msg.err != nil {
		t.Fatalf("err=%v", msg.err)
	}
	if len(msg.lines) != 1 || msg.lines[0].text != "hi" {
		t.Fatalf("lines=%+v", msg.lines)
	}
}

func TestParseBuzzPollSurfacesCLIError(t *testing.T) {
	errJSON := `{"error":"auth","message":"invalid private key"}`
	out := "CHAN x\nRC 3\nB64 " + b64s("") + "\nERRB64 " + b64s(errJSON) + "\n"
	msg := parseBuzzPoll(4, out)
	if msg.err == nil || !strings.Contains(msg.err.Error(), "invalid private key") {
		t.Fatalf("expected buzz-cli auth error, got %v", msg.err)
	}
}

func TestParseBuzzPollIgnoresMOTDAroundB64(t *testing.T) {
	payload := `[{"content":"ok","pubkey":"aa"}]`
	out := "Authorized keys warning\nCHAN c1\nRC 0\nB64 " + b64s(payload) + "\nERRB64 " + b64s("") + "\n"
	msg := parseBuzzPoll(5, out)
	if msg.err != nil || len(msg.lines) != 1 || msg.lines[0].text != "ok" {
		t.Fatalf("msg=%+v err=%v", msg, msg.err)
	}
}

func b64s(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
