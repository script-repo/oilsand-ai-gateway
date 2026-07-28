package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Buzz replaces the old Oilsand Agent Hub: a self-hosted [block/buzz] relay
// (compose stack on the gateway) plus an in-TUI chat surface driven by buzz-cli
// (REST poll/send). Nanoclaw instances join the same relay with their own keys.

const (
	buzzHTTPPort     = "3000"
	buzzOperatorName = "operator"
	buzzFeedMax      = 500
	buzzChannelName  = "oilsand"
	buzzInstallDir   = "/opt/oilsand/buzz"
	buzzImage        = "ghcr.io/block/buzz:main"
	buzzPollEvery    = 3 * time.Second
)

// buzzLine is one rendered line of the Buzz feed.
type buzzLine struct {
	from, to, text, ts string
	sys                bool
}

type buzzDeployedMsg struct {
	err       error
	opKey     string // hex private key for buzz-cli
	channelID string
}

type buzzPollMsg struct {
	gen      int
	err      error
	lines    []buzzLine
	peers    []string
	channel  string
	identity string
}

// buzzDeployScript stages upstream deploy/compose on the gateway, generates a
// LAN-friendly .env, starts the stack, installs buzz-cli from the image, and
// ensures the default "oilsand" channel exists.
func buzzDeployScript(gatewayIP string) string {
	ip := gatewayIP
	if ip == "" {
		ip = "127.0.0.1"
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(dockerBootstrap)
	b.WriteString(fmt.Sprintf(`DIR='%s'
IMG='%s'
IP='%s'
PORT='%s'
CHAN='%s'
sudo mkdir -p "$DIR"
cd "$DIR"

# Stage compose bundle from upstream (idempotent refresh of compose files only).
if [ ! -f compose.yml ]; then
  echo "[buzz] fetching deploy/compose from block/buzz…"
  sudo git clone --depth 1 --filter=blob:none --sparse https://github.com/block/buzz.git /tmp/oilsand-buzz-src 2>/dev/null \
    || { sudo rm -rf /tmp/oilsand-buzz-src; sudo git clone --depth 1 https://github.com/block/buzz.git /tmp/oilsand-buzz-src; }
  if [ -d /tmp/oilsand-buzz-src/.git ]; then
    (cd /tmp/oilsand-buzz-src && sudo git sparse-checkout set deploy/compose) 2>/dev/null || true
  fi
  sudo cp -a /tmp/oilsand-buzz-src/deploy/compose/. "$DIR"/
fi
sudo chmod +x "$DIR/run.sh" 2>/dev/null || true

# First-time .env: LAN-friendly, no CHANGE_ME left for run.sh require_env.
if [ ! -f .env ]; then
  echo "[buzz] writing .env (first-time secrets)…"
  OP_SK=$(openssl rand -hex 32)
  RELAY_SK=$(openssl rand -hex 32)
  # Owner pubkey: derive via buzz image if possible; else open membership.
  OP_PK=""
  sudo docker pull "$IMG" >/dev/null
  OP_PK=$(sudo docker run --rm --entrypoint "" -e BUZZ_PRIVATE_KEY="$OP_SK" "$IMG" \
    sh -c 'buzz users get 2>/dev/null | head -c 2000' | grep -oE '[0-9a-f]{64}' | head -n1 || true)
  {
    echo "BUZZ_IMAGE=$IMG"
    echo "BUZZ_DOMAIN=$IP"
    echo "RELAY_URL=ws://$IP:$PORT"
    echo "BUZZ_MEDIA_BASE_URL=http://$IP:$PORT/media"
    echo "BUZZ_MEDIA_SERVER_DOMAIN=$IP"
    echo "BUZZ_CORS_ORIGINS=http://$IP:$PORT"
    echo "BUZZ_REQUIRE_AUTH_TOKEN=false"
    echo "BUZZ_REQUIRE_RELAY_MEMBERSHIP=false"
    echo "BUZZ_ALLOW_NIP_OA_AUTH=true"
    echo "BUZZ_AUTO_MIGRATE=true"
    echo "BUZZ_GIT_CONFORMANCE_PROBE=true"
    echo "RUST_LOG=buzz_relay=info"
    echo "BUZZ_RELAY_PRIVATE_KEY=$RELAY_SK"
    echo "BUZZ_GIT_HOOK_HMAC_SECRET=$(openssl rand -hex 32)"
    echo "POSTGRES_DB=buzz"
    echo "POSTGRES_USER=buzz"
    echo "POSTGRES_PASSWORD=$(openssl rand -hex 16)"
    echo "REDIS_PASSWORD=$(openssl rand -hex 16)"
    echo "TYPESENSE_API_KEY=$(openssl rand -hex 16)"
    echo "BUZZ_S3_ACCESS_KEY=$(openssl rand -hex 8)"
    echo "BUZZ_S3_SECRET_KEY=$(openssl rand -hex 16)"
    echo "BUZZ_S3_BUCKET=buzz-media"
    echo "BUZZ_HTTP_PORT=$PORT"
    if [ -n "$OP_PK" ]; then echo "RELAY_OWNER_PUBKEY=$OP_PK"; else echo "RELAY_OWNER_PUBKEY=$(openssl rand -hex 32)"; fi
  } | sudo tee .env >/dev/null
  sudo chmod 600 .env
  printf '%%s\n' "$OP_SK" | sudo tee operator.key >/dev/null
  sudo chmod 600 operator.key
fi

echo "[buzz] starting compose stack…"
sudo BUZZ_COMPOSE_TLS=false ./run.sh start

# Install buzz CLI from the image onto the host.
if ! command -v buzz >/dev/null 2>&1; then
  echo "[buzz] installing buzz-cli from $IMG…"
  CID=$(sudo docker create "$IMG")
  sudo docker cp "$CID:/usr/local/bin/buzz" /usr/local/bin/buzz 2>/dev/null \
    || sudo docker cp "$CID:/app/buzz" /usr/local/bin/buzz 2>/dev/null \
    || sudo docker cp "$CID:/usr/bin/buzz" /usr/local/bin/buzz 2>/dev/null \
    || true
  sudo docker rm -f "$CID" >/dev/null 2>&1 || true
  sudo chmod +x /usr/local/bin/buzz 2>/dev/null || true
fi

# Wait for liveness.
ok=0
for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:$PORT/_liveness" >/dev/null 2>&1; then ok=1; break; fi
  sleep 2
done
if [ "$ok" -ne 1 ]; then
  echo "[buzz] ERROR: relay not live on :$PORT" >&2
  sudo docker compose --env-file .env -f compose.yml ps >&2 || true
  exit 1
fi

sudo firewall-cmd --permanent --add-port=$PORT/tcp >/dev/null 2>&1 && sudo firewall-cmd --reload >/dev/null 2>&1 || true

export BUZZ_RELAY_URL="http://127.0.0.1:$PORT"
export BUZZ_PRIVATE_KEY="$(sudo cat operator.key)"
# Ensure default channel.
CHAN_ID=""
if command -v buzz >/dev/null 2>&1; then
  LIST=$(buzz channels list 2>/dev/null || true)
  CHAN_ID=$(printf '%%s' "$LIST" | grep -i "\"name\"[[:space:]]*:[[:space:]]*\"$CHAN\"" -B5 -A5 | grep -oE '[0-9a-f-]{36}' | head -n1 || true)
  if [ -z "$CHAN_ID" ]; then
    OUT=$(buzz channels create --name "$CHAN" --type stream --visibility open 2>/dev/null || true)
    CHAN_ID=$(printf '%%s' "$OUT" | grep -oE '[0-9a-f-]{36}' | head -n1 || true)
  fi
  if [ -n "$CHAN_ID" ]; then
    buzz channels join --channel "$CHAN_ID" >/dev/null 2>&1 || true
    printf '%%s\n' "$CHAN_ID" | sudo tee channel.id >/dev/null
  fi
  buzz users set-presence --status online >/dev/null 2>&1 || true
fi

echo "[buzz] ready at http://$IP:$PORT (ws://$IP:$PORT)"
echo "OILSAND_BUZZ_OPERATOR_KEY $(sudo cat operator.key)"
echo "OILSAND_BUZZ_CHANNEL_ID $(sudo cat channel.id 2>/dev/null || true)"
`, buzzInstallDir, buzzImage, shSingle(ip), buzzHTTPPort, buzzChannelName))
	return b.String()
}

// buzzRelayHTTP is the HTTP base URL for buzz-cli on the gateway.
func (m *model) buzzRelayHTTP() string {
	host := hostFromURL(m.gateway)
	if host == "" {
		return ""
	}
	return "http://" + host + ":" + buzzHTTPPort
}

// deployBuzz installs/updates Buzz on the gateway and stores operator credentials.
func (m *model) deployBuzz() tea.Cmd {
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
		m.notice = "set an SSH password (reconnect) to deploy Buzz"
		return nil
	}
	m.hubBusy = true
	m.notice = "deploying Buzz on " + host + "…"
	user, pass := orDefault(m.sshUser, "rocky"), m.sshPass
	script := buzzDeployScript(host)
	return func() tea.Msg {
		var out string
		var err error
		if local {
			raw, e := exec.Command("bash", "-c", script).CombinedOutput()
			out, err = string(raw), e
		} else {
			client, e := dialSSH(host, user, pass)
			if e != nil {
				return buzzDeployedMsg{err: e}
			}
			defer client.Close()
			out, err = runSSH(client, script)
		}
		if err != nil {
			return buzzDeployedMsg{err: fmt.Errorf("%v: %s", err, lastNonEmptyLine(out))}
		}
		opKey := markerValue(out, "OILSAND_BUZZ_OPERATOR_KEY")
		chanID := markerValue(out, "OILSAND_BUZZ_CHANNEL_ID")
		return buzzDeployedMsg{opKey: opKey, channelID: chanID}
	}
}

func markerValue(out, key string) string {
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, key+" ") {
			return strings.TrimSpace(strings.TrimPrefix(ln, key+" "))
		}
	}
	return ""
}

// connectBuzz / buzzAutoConnect start (or refresh) the poll loop.
func (m *model) connectBuzz() tea.Cmd {
	if m.buzzRelayHTTP() == "" {
		m.notice = "connect to a gateway first — Buzz runs on the gateway host"
		return nil
	}
	if m.hubBusy {
		return nil
	}
	m.hubGen++
	m.hubOn = false
	m.notice = "refreshing Buzz feed…"
	return m.pollBuzzCmd(m.hubGen)
}

func (m *model) buzzAutoConnect() tea.Cmd {
	if m.hubBusy || m.buzzRelayHTTP() == "" {
		return nil
	}
	if m.buzzOpKey == "" {
		// Try to load operator key from the gateway on first entry.
		return m.fetchBuzzCredsCmd()
	}
	return m.pollBuzzCmd(m.hubGen)
}

func (m *model) fetchBuzzCredsCmd() tea.Cmd {
	host := hostFromURL(m.gateway)
	if host == "" {
		return nil
	}
	user, pass := orDefault(m.sshUser, "rocky"), m.sshPass
	local := isLocalHost(host)
	m.hubBusy = true
	return func() tea.Msg {
		script := "sudo cat " + buzzInstallDir + "/operator.key 2>/dev/null; echo; sudo cat " + buzzInstallDir + "/channel.id 2>/dev/null"
		var out string
		var err error
		if local {
			raw, e := exec.Command("bash", "-c", script).CombinedOutput()
			out, err = string(raw), e
		} else {
			if pass == "" {
				return buzzDeployedMsg{err: fmt.Errorf("SSH password required to read Buzz credentials")}
			}
			client, e := dialSSH(host, user, pass)
			if e != nil {
				return buzzDeployedMsg{err: e}
			}
			defer client.Close()
			out, err = runSSH(client, script)
		}
		if err != nil {
			return buzzDeployedMsg{err: fmt.Errorf("Buzz not deployed yet — ctrl+d to install: %v", err)}
		}
		parts := strings.Split(strings.TrimSpace(out), "\n")
		opKey, chanID := "", ""
		if len(parts) > 0 {
			opKey = strings.TrimSpace(parts[0])
		}
		if len(parts) > 1 {
			chanID = strings.TrimSpace(parts[len(parts)-1])
		}
		return buzzDeployedMsg{opKey: opKey, channelID: chanID}
	}
}

func (m *model) pollBuzzCmd(gen int) tea.Cmd {
	relay := m.buzzRelayHTTP()
	opKey := m.buzzOpKey
	chanID := m.buzzChannelID
	host := hostFromURL(m.gateway)
	user, pass := orDefault(m.sshUser, "rocky"), m.sshPass
	local := isLocalHost(host)
	return func() tea.Msg {
		if opKey == "" || relay == "" {
			return buzzPollMsg{gen: gen, err: fmt.Errorf("missing Buzz credentials — ctrl+d to deploy")}
		}
		script := fmt.Sprintf(`export BUZZ_RELAY_URL='%s' BUZZ_PRIVATE_KEY='%s'
if ! command -v buzz >/dev/null 2>&1 && [ -x /usr/local/bin/buzz ]; then export PATH="/usr/local/bin:$PATH"; fi
CHAN='%s'
if [ -z "$CHAN" ] && [ -f %s/channel.id ]; then CHAN=$(cat %s/channel.id); fi
echo "CHAN $CHAN"
buzz users set-presence --status online >/dev/null 2>&1 || true
if [ -n "$CHAN" ]; then
  buzz messages get --channel "$CHAN" --limit 40 2>/dev/null || echo '[]'
else
  buzz channels list 2>/dev/null || echo '[]'
fi
`, shSingle(relay), shSingle(opKey), shSingle(chanID), buzzInstallDir, buzzInstallDir)
		var out string
		var err error
		if local {
			raw, e := exec.Command("bash", "-c", script).CombinedOutput()
			out, err = string(raw), e
		} else {
			client, e := dialSSH(host, user, pass)
			if e != nil {
				return buzzPollMsg{gen: gen, err: e}
			}
			defer client.Close()
			out, err = runSSH(client, script)
		}
		if err != nil {
			return buzzPollMsg{gen: gen, err: fmt.Errorf("%v: %s", err, lastNonEmptyLine(out))}
		}
		return parseBuzzPoll(gen, out)
	}
}

func parseBuzzPoll(gen int, out string) buzzPollMsg {
	msg := buzzPollMsg{gen: gen, identity: buzzOperatorName}
	lines := strings.Split(out, "\n")
	var jsonStart int
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "CHAN ") {
			msg.channel = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "CHAN "))
			jsonStart = i + 1
			break
		}
	}
	body := strings.TrimSpace(strings.Join(lines[jsonStart:], "\n"))
	if body == "" {
		body = "[]"
	}
	// Accept either a bare array or an object with a data/messages field.
	var raw any
	if json.Unmarshal([]byte(body), &raw) != nil {
		msg.err = fmt.Errorf("could not parse buzz-cli JSON")
		return msg
	}
	arr := buzzJSONArray(raw)
	peers := map[string]struct{}{}
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		from := buzzStr(obj, "author_name", "author", "pubkey", "from", "name")
		text := buzzStr(obj, "content", "text", "body")
		ts := buzzStr(obj, "created_at", "ts", "timestamp")
		if text == "" && from == "" {
			// channel list entry
			if n := buzzStr(obj, "name"); n != "" {
				msg.peers = append(msg.peers, n)
			}
			continue
		}
		if from != "" {
			peers[from] = struct{}{}
		}
		msg.lines = append(msg.lines, buzzLine{from: orDefault(from, "?"), text: text, ts: ts})
	}
	for p := range peers {
		msg.peers = append(msg.peers, p)
	}
	return msg
}

func buzzJSONArray(raw any) []any {
	switch v := raw.(type) {
	case []any:
		return v
	case map[string]any:
		for _, key := range []string{"data", "messages", "items", "channels"} {
			if a, ok := v[key].([]any); ok {
				return a
			}
		}
	}
	return nil
}

func buzzStr(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t
				}
			case float64:
				return fmt.Sprintf("%.0f", t)
			}
		}
	}
	return ""
}

func waitBuzzPoll(gen int) tea.Cmd {
	return tea.Tick(buzzPollEvery, func(time.Time) tea.Msg {
		return buzzPollTickMsg{gen: gen}
	})
}

type buzzPollTickMsg struct{ gen int }

// sendBuzz posts the composed text via buzz-cli messages send.
func (m *model) sendBuzz() tea.Cmd {
	text := strings.TrimSpace(m.hubTA.Value())
	if text == "" {
		return nil
	}
	if m.buzzOpKey == "" || m.buzzChannelID == "" {
		m.notice = "Buzz not ready — ctrl+d to deploy, or wait for credentials"
		return nil
	}
	m.hubTA.SetValue("")
	relay := m.buzzRelayHTTP()
	opKey := m.buzzOpKey
	chanID := m.buzzChannelID
	host := hostFromURL(m.gateway)
	user, pass := orDefault(m.sshUser, "rocky"), m.sshPass
	local := isLocalHost(host)
	// Optimistic local echo.
	m.hubFeed = append(m.hubFeed, buzzLine{from: buzzOperatorName, text: text, ts: time.Now().Format(time.RFC3339)})
	m.renderBuzz()
	return func() tea.Msg {
		script := fmt.Sprintf(`export BUZZ_RELAY_URL='%s' BUZZ_PRIVATE_KEY='%s' PATH="/usr/local/bin:$PATH"
buzz messages send --channel '%s' --content '%s'
`, shSingle(relay), shSingle(opKey), shSingle(chanID), shSingle(text))
		var err error
		if local {
			_, err = exec.Command("bash", "-c", script).CombinedOutput()
		} else {
			client, e := dialSSH(host, user, pass)
			if e != nil {
				return notifyMsg("buzz send failed: " + e.Error())
			}
			defer client.Close()
			_, err = runSSH(client, script)
		}
		if err != nil {
			return notifyMsg("buzz send failed: " + err.Error())
		}
		return nil
	}
}

func (m model) handleBuzzDeployed(msg buzzDeployedMsg) (tea.Model, tea.Cmd) {
	m.hubBusy = false
	if msg.err != nil {
		m.notice = "Buzz deploy failed: " + msg.err.Error()
		return m, nil
	}
	if msg.opKey != "" {
		m.buzzOpKey = msg.opKey
	}
	if msg.channelID != "" {
		m.buzzChannelID = msg.channelID
	}
	_ = saveBuzzCfg(m.tokFile, m.buzzOpKey, m.buzzChannelID)
	m.notice = "Buzz ready on gateway — loading channel…"
	m.hubGen++
	return m, m.pollBuzzCmd(m.hubGen)
}

func (m model) handleBuzzPoll(msg buzzPollMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.hubGen {
		return m, nil
	}
	m.hubBusy = false
	if msg.err != nil {
		m.hubOn = false
		m.notice = "Buzz: " + msg.err.Error()
		m.hubFeed = append(m.hubFeed, buzzLine{sys: true, text: msg.err.Error()})
		m.renderBuzz()
		if m.section == secBuzz {
			return m, waitBuzzPoll(m.hubGen)
		}
		return m, nil
	}
	m.hubOn = true
	m.hubName = orDefault(msg.identity, buzzOperatorName)
	if msg.channel != "" && m.buzzChannelID == "" {
		m.buzzChannelID = msg.channel
		_ = saveBuzzCfg(m.tokFile, m.buzzOpKey, m.buzzChannelID)
	}
	if len(msg.lines) > 0 {
		m.hubFeed = msg.lines
	}
	m.hubPeers = msg.peers
	if len(m.hubFeed) > buzzFeedMax {
		m.hubFeed = m.hubFeed[len(m.hubFeed)-buzzFeedMax:]
	}
	m.renderBuzz()
	if m.section == secBuzz {
		return m, waitBuzzPoll(m.hubGen)
	}
	return m, nil
}

func (m *model) renderBuzz() {
	if len(m.hubFeed) == 0 {
		m.hubVP.SetContent(dimStyle.Render("No Buzz traffic yet.\n\n" +
			"Buzz is the shared workspace for humans and agents (block/buzz).\n" +
			"ctrl+d deploys the relay on the gateway; Nanoclaw instances join via buzz-cli.\n" +
			"You are \"" + buzzOperatorName + "\" on channel \"" + buzzChannelName + "\"."))
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
		if l.from != m.hubName && l.from != buzzOperatorName {
			st = lipgloss.NewStyle().Foreground(palette(buzzNameIdx(l.from))).Bold(true)
		}
		head := st.Render(l.from)
		if c := buzzClock(l.ts); c != "" {
			head += dimStyle.Render("  " + c)
		}
		b.WriteString(head + "\n" + wrap.Render(l.text) + "\n")
	}
	m.hubVP.SetContent(b.String())
	m.hubVP.GotoBottom()
}

func (m model) viewBuzz() string {
	var state string
	switch {
	case m.hubBusy:
		state = m.spin.View() + " " + warnStyle.Render("working…")
	case m.hubOn:
		state = goodStyle.Render("● connected") + dimStyle.Render(" as "+orDefault(m.hubName, buzzOperatorName))
	default:
		state = badStyle.Render("○ offline")
	}
	addr := m.buzzRelayHTTP()
	head := labelStyle.Render("Buzz") +
		dimStyle.Render("  "+orDefault(addr, "(no gateway)")+"  ") + state

	peers := "no peers yet — ctrl+d deploys Buzz, then (re)deploy Nanoclaw so instances join"
	if len(m.hubPeers) > 0 {
		peers = "seen: " + strings.Join(m.hubPeers, " · ")
	}
	hint := dimStyle.Render("enter send · ctrl+r refresh · ctrl+d deploy Buzz on gateway · esc menu")
	return lipgloss.JoinVertical(lipgloss.Left,
		head,
		m.hubVP.View(),
		dimStyle.Render(truncate(peers, maxInt(m.contentW, 10))),
		hint,
		m.hubTA.View(),
	)
}

func buzzNameIdx(name string) int {
	h := 0
	for _, r := range name {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	return h
}

func buzzClock(ts string) string {
	if i := strings.IndexByte(ts, 'T'); i >= 0 && len(ts) >= i+9 {
		return ts[i+1 : i+9]
	}
	if len(ts) >= 10 && ts[0] >= '0' && ts[0] <= '9' {
		// unix seconds
		return ts
	}
	return ts
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
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
