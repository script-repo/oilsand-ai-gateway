package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The Agent Hub is a tiny chat channel for the AI agents on the local network:
// a single TCP service (deployed on the gateway host as the oilsand-hub systemd
// unit) that relays line-delimited JSON frames between every connected client.
// Agents join it to talk to each other; the TUI joins as "operator" to monitor
// the channel and take part. The protocol is deliberately trivial so anything
// that can open a socket (a Node agent, a shell script, netcat) can join:
//
//	client -> hub : {"type":"hello","name":"nanoclaw-01","kind":"agent"}  (first frame)
//	                {"type":"msg","text":"...","to":"<optional peer>"}
//	                {"type":"ping"}
//	hub -> client : {"type":"welcome","name":"<assigned>","peers":[...],"history":[...]}
//	                {"type":"msg","from":"...","to":"...","text":"...","ts":"..."}
//	                {"type":"join","name":"..."} / {"type":"leave","name":"..."}
//	                {"type":"pong"}
//
// Frames with "to" are delivered to that peer only (plus an echo to the
// sender); everything else is broadcast. The hub keeps the last 200 messages
// and replays them on join, so the operator sees recent context. It is an
// open party line for a trusted LAN — there is no auth and no secrecy.

const (
	hubPort         = "40117"
	hubOperatorName = "operator" // the TUI's own name on the channel
	hubFeedMax      = 500        // rendered feed lines kept in memory
	hubService      = "oilsand-hub"
)

// hubFrame is one JSON line on the wire (both directions).
type hubFrame struct {
	Type    string     `json:"type"`
	Name    string     `json:"name,omitempty"`
	Kind    string     `json:"kind,omitempty"`
	From    string     `json:"from,omitempty"`
	To      string     `json:"to,omitempty"`
	Text    string     `json:"text,omitempty"`
	TS      string     `json:"ts,omitempty"`
	Peers   []string   `json:"peers,omitempty"`
	History []hubFrame `json:"history,omitempty"`
}

// hubServerPy is the hub service itself: Python 3 stdlib only, so it runs on
// any gateway VM (Rocky/Ubuntu) with zero dependencies to install.
const hubServerPy = `#!/usr/bin/env python3
"""Oilsand Agent Hub - a tiny LAN chat channel for AI agents.

One JSON object per line over TCP (port 40117). First frame must be
{"type":"hello","name":...}; then {"type":"msg","text":...,"to":optional}
frames are relayed to every client (or just "to" + an echo to the sender).
The last 200 messages are replayed to each new client as "history".
"""
import asyncio, collections, json, time

PORT = 40117
HISTORY = collections.deque(maxlen=200)
CLIENTS = {}  # name -> StreamWriter


def ts():
    return time.strftime("%Y-%m-%dT%H:%M:%S")


async def send(w, obj):
    try:
        w.write((json.dumps(obj) + "\n").encode())
        await w.drain()
    except Exception:
        pass


async def broadcast(obj, skip=None):
    for n, w in list(CLIENTS.items()):
        if n != skip:
            await send(w, obj)


async def handle(reader, writer):
    name = None
    try:
        line = await asyncio.wait_for(reader.readline(), timeout=30)
        try:
            hello = json.loads(line.decode() or "{}")
        except ValueError:
            hello = {}
        if hello.get("type") != "hello" or not str(hello.get("name", "")).strip():
            await send(writer, {"type": "error", "text": "first frame must be hello with a name"})
            return
        base = str(hello["name"]).strip()[:64]
        name, i = base, 2
        while name in CLIENTS:
            name = "%s-%d" % (base, i)
            i += 1
        CLIENTS[name] = writer
        await send(writer, {"type": "welcome", "name": name,
                            "peers": sorted(CLIENTS), "history": list(HISTORY)})
        await broadcast({"type": "join", "name": name, "ts": ts()}, skip=name)
        while True:
            line = await reader.readline()
            if not line:
                return
            try:
                msg = json.loads(line.decode())
            except ValueError:
                continue
            kind = msg.get("type")
            if kind == "ping":
                await send(writer, {"type": "pong"})
            elif kind == "msg":
                text = str(msg.get("text", ""))[:4000]
                if not text.strip():
                    continue
                to = str(msg.get("to", ""))[:64]
                out = {"type": "msg", "from": name, "to": to, "text": text, "ts": ts()}
                HISTORY.append(out)
                if to and to in CLIENTS and to != name:
                    await send(CLIENTS[to], out)
                    await send(writer, out)
                else:
                    await broadcast(out)
    except (asyncio.TimeoutError, ConnectionError):
        pass
    finally:
        if name is not None and CLIENTS.get(name) is writer:
            del CLIENTS[name]
            await broadcast({"type": "leave", "name": name, "ts": ts()})
        try:
            writer.close()
        except Exception:
            pass


async def main():
    server = await asyncio.start_server(handle, "0.0.0.0", PORT)
    async with server:
        await server.serve_forever()


if __name__ == "__main__":
    asyncio.run(main())
`

const hubUnit = `[Unit]
Description=Oilsand Agent Hub (LAN chat channel for AI agents)
After=network-online.target

[Service]
ExecStart=/usr/bin/python3 /usr/local/lib/oilsand/hub.py
Restart=always
RestartSec=2
User=nobody

[Install]
WantedBy=multi-user.target
`

// hubDeployScript installs (or updates) the hub service on a host. Idempotent:
// re-running rewrites the script + unit and restarts, so redeploys pick up new
// versions. The firewall rule is best-effort (firewalld may not be present).
func hubDeployScript() string {
	pyB64 := base64.StdEncoding.EncodeToString([]byte(hubServerPy))
	unitB64 := base64.StdEncoding.EncodeToString([]byte(hubUnit))
	return fmt.Sprintf(`set -e
echo "[hub] installing Oilsand Agent Hub (port %s)…"
sudo mkdir -p /usr/local/lib/oilsand
echo %s | base64 -d | sudo tee /usr/local/lib/oilsand/hub.py >/dev/null
echo %s | base64 -d | sudo tee /etc/systemd/system/%s.service >/dev/null
sudo systemctl daemon-reload
sudo systemctl enable --now %s
sudo systemctl restart %s
sudo firewall-cmd --permanent --add-port=%s/tcp >/dev/null 2>&1 && sudo firewall-cmd --reload >/dev/null 2>&1 || true
echo "[hub] service: $(systemctl is-active %s) on port %s"
`, hubPort, pyB64, unitB64, hubService, hubService, hubService, hubPort, hubService, hubPort)
}

// ---- TUI-side hub client -----------------------------------------------------

// hubEvent is one decoded frame (or connection-state change) from the reader
// goroutine, delivered into Update via hubEvMsg.
type hubEvent struct {
	kind    string // welcome | msg | join | leave | error | closed
	name    string
	from    string
	to      string
	text    string
	ts      string
	peers   []string
	history []hubFrame
	err     error
}

// hubLine is one rendered line of the Hub feed.
type hubLine struct {
	sys  bool // system line (join/leave/connect), rendered dim
	from string
	to   string
	text string
	ts   string
}

// Hub messages carry the dial generation so events from a stale connection
// (closed just before a reconnect) can't clobber the state of the new one.
type hubDialedMsg struct {
	gen  int
	conn net.Conn
	ch   chan hubEvent
	err  error
}
type hubEvMsg struct {
	gen int
	ev  hubEvent
}
type hubDeployedMsg struct{ err error }

// hubConnectCmd dials the hub, sends the hello frame, and starts the reader
// goroutine. The connection is handed back to the model in hubDialedMsg.
func hubConnectCmd(addr, name string, gen int) tea.Cmd {
	return func() tea.Msg {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return hubDialedMsg{gen: gen, err: err}
		}
		raw, _ := json.Marshal(hubFrame{Type: "hello", Name: name, Kind: "operator"})
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write(append(raw, '\n')); err != nil {
			conn.Close()
			return hubDialedMsg{gen: gen, err: err}
		}
		_ = conn.SetWriteDeadline(time.Time{})
		ch := make(chan hubEvent, 64)
		go hubReadLoop(conn, ch)
		return hubDialedMsg{gen: gen, conn: conn, ch: ch}
	}
}

// hubReadLoop decodes frames off the socket into ch until the connection dies,
// then emits a final "closed" event and closes the channel.
func hubReadLoop(conn net.Conn, ch chan hubEvent) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var f hubFrame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		switch f.Type {
		case "welcome":
			ch <- hubEvent{kind: "welcome", name: f.Name, peers: f.Peers, history: f.History}
		case "msg":
			ch <- hubEvent{kind: "msg", from: f.From, to: f.To, text: f.Text, ts: f.TS}
		case "join":
			ch <- hubEvent{kind: "join", name: f.Name, ts: f.TS}
		case "leave":
			ch <- hubEvent{kind: "leave", name: f.Name, ts: f.TS}
		case "error":
			ch <- hubEvent{kind: "error", err: fmt.Errorf("%s", f.Text)}
		}
	}
	ch <- hubEvent{kind: "closed", err: sc.Err()}
	close(ch)
}

func waitHub(ch chan hubEvent, gen int) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return hubEvMsg{gen: gen, ev: hubEvent{kind: "closed"}}
		}
		return hubEvMsg{gen: gen, ev: ev}
	}
}

// ---- model plumbing ----------------------------------------------------------

// hubAddr resolves the hub's address: it lives on the gateway host.
func (m *model) hubAddr() string {
	host := hostFromURL(m.gateway)
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, hubPort)
}

// connectHub (re)dials the hub, invalidating any previous connection.
func (m *model) connectHub() tea.Cmd {
	addr := m.hubAddr()
	if addr == "" {
		m.notice = "connect to a gateway first — the hub runs on the gateway host"
		return nil
	}
	if m.hubBusy {
		return nil
	}
	if m.hubConn != nil {
		m.hubConn.Close()
		m.hubConn = nil
	}
	m.hubOn = false
	m.hubBusy = true
	m.hubGen++
	m.notice = "connecting to agent hub at " + addr + "…"
	return hubConnectCmd(addr, hubOperatorName, m.hubGen)
}

// hubAutoConnect dials on entering the Hub tab when there's no live connection.
func (m *model) hubAutoConnect() tea.Cmd {
	if m.hubOn || m.hubBusy || m.hubAddr() == "" {
		return nil
	}
	return m.connectHub()
}

// deployHub installs/updates the hub service on the gateway host (over SSH, or
// locally when the gateway is this machine) and reports back via hubDeployedMsg.
func (m *model) deployHub() tea.Cmd {
	host := hostFromURL(m.gateway)
	if host == "" {
		m.notice = "connect to a gateway first"
		return nil
	}
	if m.hubBusy {
		return nil
	}
	local := isLocalHost(host)
	if !local && m.sshPass == "" {
		m.notice = "set an SSH password (reconnect) to deploy the hub"
		return nil
	}
	m.hubBusy = true
	m.notice = "deploying agent hub on " + host + "…"
	user, pass := orDefault(m.sshUser, "rocky"), m.sshPass
	return func() tea.Msg {
		script := hubDeployScript()
		if local {
			out, err := exec.Command("bash", "-c", script).CombinedOutput()
			if err != nil {
				return hubDeployedMsg{err: fmt.Errorf("%v: %s", err, lastNonEmptyLine(string(out)))}
			}
			return hubDeployedMsg{}
		}
		client, err := dialSSH(host, user, pass)
		if err != nil {
			return hubDeployedMsg{err: err}
		}
		defer client.Close()
		if out, err := runSSH(client, script); err != nil {
			return hubDeployedMsg{err: fmt.Errorf("%v: %s", err, lastNonEmptyLine(out))}
		}
		return hubDeployedMsg{}
	}
}

// sendHub posts the composed text to the channel. A leading "@name " addresses
// the message to that peer only (the hub still echoes it back to us).
func (m *model) sendHub() tea.Cmd {
	text := strings.TrimSpace(m.hubTA.Value())
	if text == "" {
		return nil
	}
	if !m.hubOn || m.hubConn == nil {
		m.notice = "not connected to the hub — ctrl+r to connect, ctrl+d to deploy it"
		return nil
	}
	m.hubTA.SetValue("")
	to := ""
	if strings.HasPrefix(text, "@") {
		if sp := strings.IndexAny(text, " \t"); sp > 1 {
			to = text[1:sp]
			text = strings.TrimSpace(text[sp+1:])
		}
	}
	if text == "" {
		return nil
	}
	conn := m.hubConn
	return func() tea.Msg {
		raw, _ := json.Marshal(hubFrame{Type: "msg", To: to, Text: text})
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write(append(raw, '\n')); err != nil {
			return notifyMsg("hub send failed: " + err.Error())
		}
		return nil
	}
}

// handleHubEvent folds one hub event into the model and re-arms the reader.
func (m model) handleHubEvent(ev hubEvent) (tea.Model, tea.Cmd) {
	switch ev.kind {
	case "welcome":
		m.hubOn = true
		m.hubName = ev.name
		m.hubPeers = ev.peers
		// The welcome replays recent history; rebuild the feed from it so a
		// reconnect doesn't duplicate lines already shown.
		m.hubFeed = m.hubFeed[:0]
		for _, h := range ev.history {
			m.hubFeed = append(m.hubFeed, hubLine{from: h.From, to: h.To, text: h.Text, ts: h.TS})
		}
		m.hubFeed = append(m.hubFeed, hubLine{sys: true, text: fmt.Sprintf("connected as %s · %d client(s) online", ev.name, len(ev.peers))})
		m.notice = "hub: connected as " + ev.name
	case "msg":
		m.hubFeed = append(m.hubFeed, hubLine{from: ev.from, to: ev.to, text: ev.text, ts: ev.ts})
	case "join":
		if !containsStr(m.hubPeers, ev.name) {
			m.hubPeers = append(m.hubPeers, ev.name)
		}
		m.hubFeed = append(m.hubFeed, hubLine{sys: true, text: ev.name + " joined", ts: ev.ts})
	case "leave":
		m.hubPeers = removeStr(m.hubPeers, ev.name)
		m.hubFeed = append(m.hubFeed, hubLine{sys: true, text: ev.name + " left", ts: ev.ts})
	case "error":
		m.notice = "hub: " + errStr(ev.err)
	case "closed":
		m.hubOn = false
		if m.hubConn != nil {
			m.hubConn.Close()
			m.hubConn = nil
		}
		m.hubPeers = nil
		m.hubFeed = append(m.hubFeed, hubLine{sys: true, text: "disconnected from hub — ctrl+r to reconnect"})
		m.renderHub()
		return m, nil // channel is closed; nothing further to wait on
	}
	if len(m.hubFeed) > hubFeedMax {
		m.hubFeed = m.hubFeed[len(m.hubFeed)-hubFeedMax:]
	}
	m.renderHub()
	return m, waitHub(m.hubCh, m.hubGen)
}

// ---- rendering ---------------------------------------------------------------

// renderHub rebuilds the Hub viewport from the feed.
func (m *model) renderHub() {
	if len(m.hubFeed) == 0 {
		m.hubVP.SetContent(dimStyle.Render("No hub traffic yet.\n\n" +
			"The Agent Hub is a shared chat channel for the deployed agents.\n" +
			"ctrl+d installs it on the gateway host; agents join it over TCP\n" +
			"(port " + hubPort + ", JSON lines — see README). You are \"" + hubOperatorName + "\"."))
		return
	}
	wrap := lipgloss.NewStyle().Width(maxInt(m.contentW, 20))
	var b strings.Builder
	for _, l := range m.hubFeed {
		if l.sys {
			b.WriteString(dimStyle.Render("· "+l.text) + "\n")
			continue
		}
		st := youStyle
		if l.from != m.hubName {
			st = lipgloss.NewStyle().Foreground(palette(hubNameIdx(l.from))).Bold(true)
		}
		head := st.Render(l.from)
		if l.to != "" {
			head += dimStyle.Render(" → " + l.to)
		}
		if c := hubClock(l.ts); c != "" {
			head += dimStyle.Render("  " + c)
		}
		b.WriteString(head + "\n" + wrap.Render(l.text) + "\n")
	}
	m.hubVP.SetContent(b.String())
	m.hubVP.GotoBottom()
}

func (m model) viewHub() string {
	var state string
	switch {
	case m.hubBusy:
		state = m.spin.View() + " " + warnStyle.Render("working…")
	case m.hubOn:
		state = goodStyle.Render("● connected") + dimStyle.Render(" as "+m.hubName)
	default:
		state = badStyle.Render("○ offline")
	}
	head := labelStyle.Render("Agent Hub") +
		dimStyle.Render("  "+orDefault(m.hubAddr(), "(no gateway)")+"  ") + state

	peers := "no agents on the channel — deployed Nanoclaw instances receive the hub address via OILSAND_HUB_* env"
	if others := removeStr(append([]string(nil), m.hubPeers...), m.hubName); len(others) > 0 {
		peers = "on channel: " + strings.Join(others, " · ")
	}
	hint := dimStyle.Render("enter send · @name direct message · ctrl+r reconnect · ctrl+d deploy hub on gateway · esc menu")
	return lipgloss.JoinVertical(lipgloss.Left,
		head,
		m.hubVP.View(),
		dimStyle.Render(truncate(peers, maxInt(m.contentW, 10))),
		hint,
		m.hubTA.View(),
	)
}

// hubNameIdx hashes a peer name to a stable palette index.
func hubNameIdx(name string) int {
	h := 0
	for _, r := range name {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	return h
}

// hubClock extracts the HH:MM:SS part of a hub timestamp for compact display.
func hubClock(ts string) string {
	if i := strings.IndexByte(ts, 'T'); i >= 0 && len(ts) >= i+9 {
		return ts[i+1 : i+9]
	}
	return ts
}

func removeStr(list []string, s string) []string {
	out := list[:0]
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// lastNonEmptyLine returns the last non-blank line of command output, for
// compact error notices.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
