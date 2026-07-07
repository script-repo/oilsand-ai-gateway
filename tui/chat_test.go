package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestChatMessagesMemory(t *testing.T) {
	m := &model{}
	m.history = []chatTurn{
		{role: roleUser, content: "first question"},
		{role: roleBot, content: "first answer"},
		{role: roleUser, content: "second question"},
	}
	msgs := m.chatMessages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" || msgs[2].Role != "user" {
		t.Fatalf("roles wrong: %+v", msgs)
	}
	if msgs[2].Content != "second question" {
		t.Fatalf("last message must be the latest user turn, got %q", msgs[2].Content)
	}
}

func TestChatMessagesTurnCap(t *testing.T) {
	m := &model{}
	for i := 0; i < chatHistoryMaxTurns*2; i++ {
		role := roleUser
		if i%2 == 1 {
			role = roleBot
		}
		m.history = append(m.history, chatTurn{role: role, content: "turn"})
	}
	if got := len(m.chatMessages()); got != chatHistoryMaxTurns {
		t.Fatalf("expected history capped at %d turns, got %d", chatHistoryMaxTurns, got)
	}
}

func TestChatMessagesCharCap(t *testing.T) {
	m := &model{}
	big := strings.Repeat("x", chatHistoryMaxChars) // older turn that blows the budget
	m.history = []chatTurn{
		{role: roleUser, content: big},
		{role: roleBot, content: big},
		{role: roleUser, content: "latest"},
	}
	msgs := m.chatMessages()
	// The turn that exceeds the budget is dropped too, so only "latest" remains.
	if len(msgs) != 1 || msgs[0].Content != "latest" {
		t.Fatalf("expected only the latest turn after trimming, got %d msgs", len(msgs))
	}

	// A latest turn that alone exceeds the budget must still be sent.
	m.history = []chatTurn{{role: roleUser, content: big + big}}
	if msgs := m.chatMessages(); len(msgs) != 1 {
		t.Fatalf("oversized latest turn must survive, got %d msgs", len(msgs))
	}
}

func TestNewChatSession(t *testing.T) {
	m := &model{}
	m.history = []chatTurn{{role: roleUser, content: "hello"}}
	m.lastTokS, m.lastTTFT = 12.3, 456
	m.newChatSession()
	if len(m.history) != 0 || m.lastTokS != 0 || m.lastTTFT != 0 {
		t.Fatalf("session not cleared: %+v", m.history)
	}
	// While streaming the session must be preserved.
	m.history = []chatTurn{{role: roleUser, content: "hello"}}
	m.streaming = true
	m.newChatSession()
	if len(m.history) != 1 {
		t.Fatal("newChatSession must not clear history mid-stream")
	}
}

func TestExtractURLs(t *testing.T) {
	text := "compare https://example.com/a, and http://foo.io/b?q=1) also https://example.com/a again " +
		"plus https://one.dev https://two.dev https://three.dev https://four.dev"
	urls := extractURLs(text)
	if len(urls) != webFetchMaxURLs {
		t.Fatalf("expected cap of %d urls, got %d: %v", webFetchMaxURLs, len(urls), urls)
	}
	if urls[0] != "https://example.com/a" {
		t.Fatalf("trailing punctuation not stripped: %q", urls[0])
	}
	if urls[1] != "http://foo.io/b?q=1" {
		t.Fatalf("query URL mangled: %q", urls[1])
	}
	if len(extractURLs("no links here")) != 0 {
		t.Fatal("found URLs where there are none")
	}
}

func TestHTMLToText(t *testing.T) {
	html := `<html><head><title>t</title><style>body{color:red}</style></head>
<body><script>var x=1;</script><h1>Header</h1><p>Hello &amp; welcome</p><div>line</div></body></html>`
	out := htmlToText(html)
	if strings.Contains(out, "var x") || strings.Contains(out, "color:red") {
		t.Fatalf("script/style leaked into text: %q", out)
	}
	if !strings.Contains(out, "Header") || !strings.Contains(out, "Hello & welcome") {
		t.Fatalf("content missing or entities not decoded: %q", out)
	}
}

func TestNanoclawCatalogAndScript(t *testing.T) {
	a, ok := agentByName("Nanoclaw")
	if !ok {
		t.Fatal("Nanoclaw missing from agent catalog")
	}
	if !a.deployable || !a.container || a.target != "worker" {
		t.Fatalf("Nanoclaw def wrong: %+v", a)
	}

	m := &model{gateway: "http://10.0.0.1:40114", token: "tok-123", defModel: "rnj-1"}
	script := m.nanoclawDeployScript(3)
	for _, want := range []string{
		"seq 1 3",                               // three instances
		"nanoclaw-$(printf",                     // auto-incremented container names
		nanoclawImage,                           // shared per-worker image
		"http://10.0.0.1:40114/olla/openai/v1",  // Olla endpoint wired in
		"OPENAI_API_KEY='tok-123'",              // token wired in
		"NANOCLAW_MODEL='rnj-1'",                // default model wired in
		"docker run -d --restart unless-stopped", // isolated containers
		"-v \"oilsand-$NAME:",                   // per-instance state volume
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("deploy script missing %q:\n%s", want, script)
		}
	}
	// One instance is the floor even for bad input.
	if !strings.Contains(m.nanoclawDeployScript(0), "seq 1 1") {
		t.Fatal("instance floor of 1 not applied")
	}
}

func TestNanoclawDockerfileBuildsDist(t *testing.T) {
	// Upstream "start" runs dist/index.js, so the image must compile the
	// TypeScript source (pnpm install + build) before it can run.
	for _, want := range []string{"pnpm install", "pnpm run build", `CMD ["node", "dist/index.js"]`} {
		if !strings.Contains(nanoclawDockerfile, want) {
			t.Fatalf("Dockerfile missing %q:\n%s", want, nanoclawDockerfile)
		}
	}
}

func TestNanoclawHostsTracking(t *testing.T) {
	m := newModel("http://10.0.0.1:40114", "rocky", "pw")
	m.tokFile = filepath.Join(t.TempDir(), "tui.json")
	m.agentReg = map[string]string{}
	m.agentHosts = map[string][]string{}
	// Deploy to two workers, then re-deploy on the first (more instances).
	for _, h := range []string{"10.0.0.4", "10.0.0.5", "10.0.0.4"} {
		m = drive(m, agentRegisteredMsg{agent: "Nanoclaw", host: h})
	}
	hosts := m.nanoclawHosts()
	if len(hosts) != 2 || hosts[0] != "10.0.0.4" || hosts[1] != "10.0.0.5" {
		t.Fatalf("expected both workers tracked (deduped), got %v", hosts)
	}
	if m.agentReg["Nanoclaw"] != "10.0.0.4" {
		t.Fatalf("most-recent host wrong: %q", m.agentReg["Nanoclaw"])
	}
	// Legacy configs (single-host map only) still yield the one known host.
	legacy := &model{agentReg: map[string]string{"Nanoclaw": "10.0.0.9"}}
	if hosts := legacy.nanoclawHosts(); len(hosts) != 1 || hosts[0] != "10.0.0.9" {
		t.Fatalf("legacy fallback broken: %v", hosts)
	}
}
