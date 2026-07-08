package main

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// stubHub is a minimal in-process hub speaking the wire protocol, so the Go
// client (dial, hello, event decoding) is exercised against a real socket.
func stubHub(t *testing.T) (addr string, gotHello chan hubFrame, srvConn chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	gotHello = make(chan hubFrame, 1)
	srvConn = make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		sc := bufio.NewScanner(conn)
		if !sc.Scan() {
			return
		}
		var hello hubFrame
		_ = json.Unmarshal(sc.Bytes(), &hello)
		gotHello <- hello
		welcome, _ := json.Marshal(hubFrame{
			Type: "welcome", Name: hello.Name,
			Peers:   []string{hello.Name, "nanoclaw-01"},
			History: []hubFrame{{Type: "msg", From: "nanoclaw-01", Text: "old news", TS: "2026-07-07T10:00:00"}},
		})
		conn.Write(append(welcome, '\n'))
		srvConn <- conn
	}()
	return ln.Addr().String(), gotHello, srvConn
}

func recvEvent(t *testing.T, ch chan hubEvent) hubEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for hub event")
		return hubEvent{}
	}
}

func TestHubClientHandshakeAndEvents(t *testing.T) {
	addr, gotHello, srvConn := stubHub(t)

	msg := hubConnectCmd(addr, "operator", 1)()
	dialed, ok := msg.(hubDialedMsg)
	if !ok || dialed.err != nil {
		t.Fatalf("dial failed: %#v", msg)
	}
	defer dialed.conn.Close()

	hello := <-gotHello
	if hello.Type != "hello" || hello.Name != "operator" || hello.Kind != "operator" {
		t.Fatalf("bad hello frame: %+v", hello)
	}

	// welcome carries the assigned name, roster, and replayed history.
	ev := recvEvent(t, dialed.ch)
	if ev.kind != "welcome" || ev.name != "operator" || len(ev.peers) != 2 || len(ev.history) != 1 {
		t.Fatalf("bad welcome event: %+v", ev)
	}

	// A broadcast message must decode into a msg event.
	conn := <-srvConn
	raw, _ := json.Marshal(hubFrame{Type: "msg", From: "nanoclaw-01", Text: "hello ops", TS: "2026-07-07T10:00:05"})
	conn.Write(append(raw, '\n'))
	ev = recvEvent(t, dialed.ch)
	if ev.kind != "msg" || ev.from != "nanoclaw-01" || ev.text != "hello ops" {
		t.Fatalf("bad msg event: %+v", ev)
	}

	// Closing the server side must surface a closed event, then close the channel.
	conn.Close()
	if ev = recvEvent(t, dialed.ch); ev.kind != "closed" {
		t.Fatalf("expected closed event, got %+v", ev)
	}
	if _, open := <-dialed.ch; open {
		t.Fatal("event channel still open after close")
	}
}

func TestHubModelFlow(t *testing.T) {
	addr, _, _ := stubHub(t)

	m := newModel("http://10.0.0.1:40114", "rocky", "pw")
	m = drive(m, tea.WindowSizeMsg{Width: 120, Height: 40})

	// connectHub dials whatever hubAddr() says; point it at the stub.
	host, port, _ := net.SplitHostPort(addr)
	m.gateway = "http://" + net.JoinHostPort(host, port) // hostFromURL strips the port; hub port differs, so dial directly:
	cmd := hubConnectCmd(addr, hubOperatorName, m.hubGen+1)
	m.hubGen++
	m.hubBusy = true

	msg := cmd()
	m = drive(m, msg)
	if m.hubBusy || m.hubConn == nil {
		t.Fatalf("model did not adopt hub connection: busy=%v conn=%v", m.hubBusy, m.hubConn)
	}

	// Pump the welcome event through the Update loop.
	ev := recvEvent(t, m.hubCh)
	m = drive(m, hubEvMsg{gen: m.hubGen, ev: ev})
	if !m.hubOn || m.hubName != "operator" {
		t.Fatalf("welcome not applied: on=%v name=%q", m.hubOn, m.hubName)
	}
	if len(m.hubFeed) == 0 || m.hubFeed[0].text != "old news" {
		t.Fatalf("history not replayed into feed: %+v", m.hubFeed)
	}

	// Stale-generation events must be dropped, not applied.
	m = drive(m, hubEvMsg{gen: m.hubGen - 1, ev: hubEvent{kind: "closed"}})
	if !m.hubOn {
		t.Fatal("stale closed event disconnected the live hub session")
	}

	// The Hub section must render with a live feed.
	m.section = secHub
	m.zone = zoneContent
	out := m.View()
	if !strings.Contains(out, "Agent Hub") {
		t.Fatalf("hub view missing header:\n%s", out)
	}
}

func TestHubSendParsesDirectMessages(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()

	m := newModel("http://10.0.0.1:40114", "rocky", "pw")
	m.hubOn = true
	m.hubConn = cli
	m.hubName = "operator"
	m.hubTA.SetValue("@nanoclaw-01 restart yourself")

	cmd := m.sendHub()
	if cmd == nil {
		t.Fatal("sendHub returned no command")
	}
	go cmd()
	var f hubFrame
	srv.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := json.NewDecoder(srv).Decode(&f); err != nil {
		t.Fatal(err)
	}
	if f.Type != "msg" || f.To != "nanoclaw-01" || f.Text != "restart yourself" {
		t.Fatalf("bad DM frame: %+v", f)
	}
	if m.hubTA.Value() != "" {
		t.Fatal("composer not cleared after send")
	}
}

func TestHubDeployScript(t *testing.T) {
	s := hubDeployScript()
	for _, want := range []string{
		"oilsand-hub",            // service name
		"systemctl enable --now", // starts on boot
		"systemctl restart",      // redeploys pick up new versions
		hubPort + "/tcp",         // firewall opening
		"base64 -d",              // embedded server + unit
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("hub deploy script missing %q:\n%s", want, s)
		}
	}
}

func TestHubSectionWiring(t *testing.T) {
	if sections[secHub].name != "Hub" {
		t.Fatalf("secHub is %q", sections[secHub].name)
	}
	m := newModel("http://10.0.0.1:40114", "rocky", "pw")
	m = drive(m, tea.WindowSizeMsg{Width: 120, Height: 40})

	// '6' jumps to the Hub, '0' reaches the tenth section (Update).
	m = drive(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'6'}})
	if m.section != secHub || m.zone != zoneContent {
		t.Fatalf("6 did not jump to Hub: section=%v zone=%v", m.section, m.zone)
	}
	if !m.isTyping() {
		t.Fatal("hub content zone should capture typing")
	}
	m.zone = zoneSidebar
	m = drive(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'0'}})
	if m.section != secUpdate {
		t.Fatalf("0 did not jump to Update: section=%v", m.section)
	}
}

func TestNanoclawInstanceParsingAndView(t *testing.T) {
	out := "Warning: Permanently added 'x' to hosts.\n" +
		"nanoclaw-01\tUp 12 minutes\toilsand/nanoclaw:latest\n" +
		"nanoclaw-02\tExited (1) 2 hours ago\toilsand/nanoclaw:latest\n" +
		"not-a-nanoclaw\tUp\tother:img\n"
	rows := parseNanoclawPS("10.0.0.4", out)
	if len(rows) != 2 {
		t.Fatalf("expected 2 instances, got %+v", rows)
	}
	if rows[0].name != "nanoclaw-01" || rows[0].status != "Up 12 minutes" || rows[0].host != "10.0.0.4" {
		t.Fatalf("bad first row: %+v", rows[0])
	}

	m := newModel("http://10.0.0.1:40114", "rocky", "pw")
	m = drive(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = drive(m, nanoclawInstancesMsg{rows: rows, hosts: 1, errs: []string{"10.0.0.9: ssh connect failed"}})
	if m.nanoInstBusy {
		t.Fatal("busy flag not cleared")
	}
	m.section = secAgents
	m.zone = zoneContent
	out2 := m.View()
	for _, want := range []string{"Nanoclaw instances", "nanoclaw-01", "10.0.0.9"} {
		if !strings.Contains(out2, want) {
			t.Fatalf("agents view missing %q:\n%s", want, out2)
		}
	}
}

func TestNanoclawDeployWiresHubEnv(t *testing.T) {
	m := &model{gateway: "http://10.0.0.1:40114", token: "tok", defModel: "rnj-1"}
	script := m.nanoclawDeployScript(1)
	for _, want := range []string{
		"OILSAND_HUB_HOST='10.0.0.1'",
		"OILSAND_HUB_PORT='" + hubPort + "'",
		`OILSAND_HUB_NAME="$NAME"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("deploy script missing %q:\n%s", want, script)
		}
	}
}
