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
	out := "CHAN abc-def\n" + `[{"author_name":"nanoclaw-01","content":"hello","created_at":"2026-01-01T12:00:00Z"}]`
	msg := parseBuzzPoll(1, out)
	if msg.channel != "abc-def" {
		t.Fatalf("channel=%q", msg.channel)
	}
	if len(msg.lines) != 1 || msg.lines[0].text != "hello" {
		t.Fatalf("lines=%+v", msg.lines)
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
