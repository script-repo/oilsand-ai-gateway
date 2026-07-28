package main

import (
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
	out := "CHAN abc-def\nJSON_BEGIN\n" +
		`[{"author_name":"nanoclaw-01","content":"hello","created_at":"2026-01-01T12:00:00Z"}]` +
		"\nJSON_END\n"
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
	out := "CHAN uuid-here\nJSON_BEGIN\n[]\nJSON_END\n"
	msg := parseBuzzPoll(2, out)
	if msg.err != nil {
		t.Fatalf("empty feed should not error: %v", msg.err)
	}
	if msg.channel != "uuid-here" || len(msg.lines) != 0 {
		t.Fatalf("msg=%+v", msg)
	}
}

func TestParseBuzzPollExtractsFromNoise(t *testing.T) {
	// MOTD / banner before markers (or legacy poll without markers).
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
	out := "CHAN\nJSON_BEGIN\n[]\nJSON_END\n" +
		`BUZZ_ERR {"error":"auth","message":"invalid private key"}` + "\n"
	// empty CHAN + empty messages is OK; with BUZZ_ERR and empty useful data we
	// only surface err when JSON body itself is bad. Force bad body:
	out = "CHAN x\nJSON_BEGIN\nnot-json\nJSON_END\n" +
		`BUZZ_ERR {"error":"auth","message":"invalid private key"}` + "\n"
	msg := parseBuzzPoll(4, out)
	if msg.err == nil || !strings.Contains(msg.err.Error(), "invalid private key") {
		t.Fatalf("expected buzz-cli auth error, got %v", msg.err)
	}
}

func TestMarkerValue(t *testing.T) {
	out := "noise\nOILSAND_BUZZ_OPERATOR_KEY deadbeef\nOILSAND_BUZZ_CHANNEL_ID uuid-here\n"
	if got := markerValue(out, "OILSAND_BUZZ_OPERATOR_KEY"); got != "deadbeef" {
		t.Fatalf("got %q", got)
	}
	if got := markerValue(out, "OILSAND_BUZZ_CHANNEL_ID"); got != "uuid-here" {
		t.Fatalf("got %q", got)
	}
}
