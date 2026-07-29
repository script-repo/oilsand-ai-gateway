package main

import (
	"encoding/base64"
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

// buzzHostDepsBootstrap installs tools the Buzz deploy script needs on a bare
// gateway image (git for compose staging, openssl for secrets, curl for the
// liveness wait). Rocky/RHEL and Ubuntu/Debian.
const buzzHostDepsBootstrap = `echo "[buzz] ensuring git, openssl, curl…"
. /etc/os-release 2>/dev/null || true
need_git=0; need_ssl=0; need_curl=0
command -v git >/dev/null 2>&1 || need_git=1
command -v openssl >/dev/null 2>&1 || need_ssl=1
command -v curl >/dev/null 2>&1 || need_curl=1
if [ "$need_git$need_ssl$need_curl" != "000" ]; then
  case "${ID:-}" in
    ubuntu|debian)
      sudo apt-get update -y >/dev/null 2>&1 || true
      pkgs=""
      [ "$need_git" = 1 ] && pkgs="$pkgs git"
      [ "$need_ssl" = 1 ] && pkgs="$pkgs openssl"
      [ "$need_curl" = 1 ] && pkgs="$pkgs curl"
      sudo apt-get install -y $pkgs
      ;;
    *)
      pkgs=""
      [ "$need_git" = 1 ] && pkgs="$pkgs git"
      [ "$need_ssl" = 1 ] && pkgs="$pkgs openssl"
      [ "$need_curl" = 1 ] && pkgs="$pkgs curl"
      sudo dnf install -y $pkgs 2>/dev/null || sudo yum install -y $pkgs
      ;;
  esac
fi
command -v git >/dev/null 2>&1 || { echo "[buzz] ERROR: git still missing after install" >&2; exit 1; }
`

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
	// Gateway images often ship without git/openssl/curl; compose staging and
	// first-time .env generation need them before any clone or rand work.
	b.WriteString(buzzHostDepsBootstrap)
	b.WriteString(fmt.Sprintf(`DIR='%s'
IMG='%s'
# Trim IP (gateway URLs / hostname -I can leave whitespace).
IP="$(printf '%%s' '%s' | tr -d '[:space:]')"
PORT='%s'
CHAN='%s'
# Canonical host for community tenancy. Browser/desktop Host for :3000 is
# "IP:3000"; RELAY_URL must yield that same authority or clients get
# "no community is configured for this host".
HOST_AUTH="$IP:$PORT"
sudo mkdir -p "$DIR"
cd "$DIR"

# Stage compose bundle from upstream.
if [ ! -f compose.yml ]; then
  echo "[buzz] fetching deploy/compose from block/buzz…"
  sudo rm -rf /tmp/oilsand-buzz-src
  git clone --depth 1 --filter=blob:none --sparse https://github.com/block/buzz.git /tmp/oilsand-buzz-src 2>/dev/null \
    || { rm -rf /tmp/oilsand-buzz-src; git clone --depth 1 https://github.com/block/buzz.git /tmp/oilsand-buzz-src; }
  if [ -d /tmp/oilsand-buzz-src/.git ]; then
    (cd /tmp/oilsand-buzz-src && git sparse-checkout set deploy/compose) 2>/dev/null || true
  fi
  if [ ! -d /tmp/oilsand-buzz-src/deploy/compose ]; then
    echo "[buzz] ERROR: deploy/compose missing from block/buzz clone" >&2
    exit 1
  fi
  sudo cp -a /tmp/oilsand-buzz-src/deploy/compose/. "$DIR"/
fi
sudo chmod +x "$DIR/run.sh" 2>/dev/null || true

sudo docker pull "$IMG" >/dev/null

# Owner keypair via buzz-admin generate-key (image has buzz-admin, NOT buzz-cli).
# Output shape:
#   Public key:  <64-hex>
#   Secret key:  <64-hex or nsec1…>
gen_owner() {
  OUT=$(sudo docker run --rm --entrypoint buzz-admin "$IMG" generate-key 2>&1 || true)
  OP_PK=$(printf '%%s\n' "$OUT" | sed -n 's/.*[Pp]ublic key:[[:space:]]*//p' | head -n1 | tr -d '[:space:]' | grep -oE '[0-9a-fA-F]{64}' | tr 'A-F' 'a-f' | head -n1 || true)
  OP_SK=$(printf '%%s\n' "$OUT" | sed -n 's/.*[Ss]ecret key:[[:space:]]*//p' | head -n1 | tr -d '[:space:]' || true)
  # Secret may be hex or nsec1…; accept either.
  if ! printf '%%s' "$OP_SK" | grep -qE '^([0-9a-fA-F]{64}|nsec1[a-z0-9]+)$'; then
    OP_SK=""
  fi
  if [ -z "$OP_SK" ] || [ -z "$OP_PK" ]; then
    echo "[buzz] WARNING: could not parse buzz-admin generate-key output; generating hex sk only" >&2
    printf '%%s\n' "$OUT" | tail -n 20 >&2 || true
    OP_SK=$(openssl rand -hex 32)
    OP_PK=""
  fi
}

# First-time secrets.
if [ ! -f .env ]; then
  echo "[buzz] writing .env (first-time secrets)…"
  gen_owner
  RELAY_SK=$(openssl rand -hex 32)
  {
    echo "BUZZ_IMAGE=$IMG"
    echo "BUZZ_DOMAIN=$HOST_AUTH"
    echo "RELAY_URL=ws://$HOST_AUTH"
    echo "BUZZ_MEDIA_BASE_URL=http://$HOST_AUTH/media"
    echo "BUZZ_MEDIA_SERVER_DOMAIN=$IP"
    echo "BUZZ_CORS_ORIGINS=http://$HOST_AUTH,http://$IP:$PORT,http://127.0.0.1:$PORT"
    echo "BUZZ_REQUIRE_AUTH_TOKEN=false"
    echo "BUZZ_REQUIRE_RELAY_MEMBERSHIP=false"
    echo "BUZZ_ALLOW_NIP_OA_AUTH=true"
    echo "BUZZ_AUTO_MIGRATE=true"
    echo "BUZZ_GIT_CONFORMANCE_PROBE=true"
    echo "RUST_LOG=buzz_relay=info,buzz_db=info"
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
    if [ -n "$OP_PK" ]; then
      echo "RELAY_OWNER_PUBKEY=$OP_PK"
      echo "RELAY_OPERATOR_PUBKEYS=$OP_PK"
    fi
  } | sudo tee .env >/dev/null
  sudo chmod 600 .env
  printf '%%s\n' "$OP_SK" | sudo tee operator.key >/dev/null
  sudo chmod 600 operator.key
else
  echo "[buzz] updating community host in existing .env → $HOST_AUTH"
  # Keep secrets; rewrite host-derived URLs so browser Host matches community.
  sudo cp .env .env.bak 2>/dev/null || true
  sudo sed -i \
    -e "s|^BUZZ_DOMAIN=.*|BUZZ_DOMAIN=$HOST_AUTH|" \
    -e "s|^RELAY_URL=.*|RELAY_URL=ws://$HOST_AUTH|" \
    -e "s|^BUZZ_MEDIA_BASE_URL=.*|BUZZ_MEDIA_BASE_URL=http://$HOST_AUTH/media|" \
    -e "s|^BUZZ_MEDIA_SERVER_DOMAIN=.*|BUZZ_MEDIA_SERVER_DOMAIN=$IP|" \
    -e "s|^BUZZ_CORS_ORIGINS=.*|BUZZ_CORS_ORIGINS=http://$HOST_AUTH,http://$IP:$PORT,http://127.0.0.1:$PORT|" \
    -e "s|^BUZZ_REQUIRE_AUTH_TOKEN=.*|BUZZ_REQUIRE_AUTH_TOKEN=false|" \
    -e "s|^BUZZ_REQUIRE_RELAY_MEMBERSHIP=.*|BUZZ_REQUIRE_RELAY_MEMBERSHIP=false|" \
    -e "s|^BUZZ_AUTO_MIGRATE=.*|BUZZ_AUTO_MIGRATE=true|" \
    -e "s|^BUZZ_HTTP_PORT=.*|BUZZ_HTTP_PORT=$PORT|" \
    .env
  # Ensure no leftover CHANGE_ME (run.sh refuses to start).
  if grep -Eq 'CHANGE_ME' .env; then
    echo "[buzz] ERROR: .env still has CHANGE_ME placeholders — remove $DIR/.env and redeploy" >&2
    exit 1
  fi
fi

echo "[buzz] starting compose stack…"
sudo BUZZ_COMPOSE_TLS=false ./run.sh start

# Wait for liveness.
ok=0
for _ in $(seq 1 90); do
  if curl -fsS "http://127.0.0.1:$PORT/_liveness" >/dev/null 2>&1; then ok=1; break; fi
  if curl -fsS --max-time 3 "http://127.0.0.1:$PORT/" >/dev/null 2>&1; then ok=1; break; fi
  sleep 2
done
if [ "$ok" -ne 1 ]; then
  echo "[buzz] ERROR: relay not live on :$PORT" >&2
  sudo docker compose --env-file .env -f compose.yml ps >&2 || true
  sudo docker compose --env-file .env -f compose.yml logs --tail 80 relay >&2 || true
  exit 1
fi

# Restart relay so ensure_configured_community runs with the rewritten RELAY_URL.
echo "[buzz] restarting relay to seed community for host $HOST_AUTH…"
sudo BUZZ_COMPOSE_TLS=false ./run.sh restart || sudo docker compose --env-file .env -f compose.yml up -d --force-recreate relay
sleep 8

# Belt-and-suspenders: insert community host rows directly in Postgres for every
# Host header clients actually send (IP:port, bare IP, localhost variants).
# ensure_configured_community does the same INSERT; this covers cases where
# startup seed was skipped (empty authority / migrate race).
echo "[buzz] ensuring communities.host rows for client Host headers…"
set -a
# shellcheck disable=SC1091
. ./.env 2>/dev/null || true
set +a
PGUSER="${POSTGRES_USER:-buzz}"
PGDB="${POSTGRES_DB:-buzz}"
PGPASS="${POSTGRES_PASSWORD:-}"
seed_host() {
  h="$1"
  [ -n "$h" ] || return 0
  sudo docker compose --env-file .env -f compose.yml exec -T -e PGPASSWORD="$PGPASS" postgres \
    psql -U "$PGUSER" -d "$PGDB" -v ON_ERROR_STOP=1 \
    -c "INSERT INTO communities (host) VALUES ('$h') ON CONFLICT (lower(host)) DO UPDATE SET host = communities.host;" \
    >/dev/null 2>&1 \
    && echo "[buzz] community host ok: $h" \
    || echo "[buzz] WARNING: could not upsert communities.host=$h (migrations may still be running)" >&2
}
seed_host "$HOST_AUTH"
seed_host "$IP"
seed_host "127.0.0.1:$PORT"
seed_host "localhost:$PORT"
seed_host "127.0.0.1"
seed_host "localhost"

sudo firewall-cmd --permanent --add-port=$PORT/tcp >/dev/null 2>&1 && sudo firewall-cmd --reload >/dev/null 2>&1 || true

CHAN_ID=""
if [ -f channel.id ]; then CHAN_ID=$(sudo cat channel.id 2>/dev/null | tr -d '\r\n'); fi
if [ -z "$CHAN_ID" ]; then
  CHAN_ID=$(cat /proc/sys/kernel/random/uuid 2>/dev/null || openssl rand -hex 16 | sed 's/\(........\)\(....\)\(....\)\(....\)\(............\)/\1-\2-\3-\4-\5/')
  printf '%%s\n' "$CHAN_ID" | sudo tee channel.id >/dev/null
fi

echo "[buzz] ready"
echo "[buzz] desktop/browser Relay URL must be exactly:  ws://$HOST_AUTH"
echo "[buzz] web UI:  http://$IP:$PORT"
echo "[buzz] if you still see 'no community', wipe volumes once and redeploy:"
echo "[buzz]   cd $DIR && sudo docker compose --env-file .env -f compose.yml down -v && sudo rm -f .env && re-run ctrl+d"
echo "OILSAND_BUZZ_OPERATOR_KEY $(sudo cat operator.key | tr -d '\r\n')"
echo "OILSAND_BUZZ_CHANNEL_ID $(sudo cat channel.id 2>/dev/null | tr -d '\r\n')"
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
		// Probe relay HTTP first (always works). buzz-cli is optional — the
		// public ghcr.io/block/buzz image ships buzz-relay/admin only.
		script := fmt.Sprintf(`set +e
export PATH="/usr/local/bin:$PATH"
export BUZZ_RELAY_URL='%s'
export BUZZ_PRIVATE_KEY='%s'
IMG='%s'
DIR='%s'
CHAN='%s'
PORT='%s'
if [ -z "$CHAN" ] && [ -f "$DIR/channel.id" ]; then CHAN=$(sudo cat "$DIR/channel.id" 2>/dev/null | tr -d '\r\n'); fi
if [ -z "$CHAN" ] && [ -r "$DIR/channel.id" ]; then CHAN=$(tr -d '\r\n' < "$DIR/channel.id"); fi
echo "CHAN $CHAN"
# Connectivity: liveness on the app port.
if curl -fsS "http://127.0.0.1:$PORT/_liveness" >/dev/null 2>&1 \
   || curl -fsS --max-time 3 "http://127.0.0.1:$PORT/" >/dev/null 2>&1; then
  echo "RELAY_UP 1"
else
  echo "RELAY_UP 0"
fi
# Optional message feed if a real buzz-cli exists on the host.
OUTF=$(mktemp); ERRF=$(mktemp)
RC=0
if command -v buzz >/dev/null 2>&1 || [ -x /usr/local/bin/buzz ]; then
  BUZZ_BIN=$(command -v buzz 2>/dev/null || echo /usr/local/bin/buzz)
  if [ -n "$CHAN" ]; then
    "$BUZZ_BIN" messages get --channel "$CHAN" --limit 40 >"$OUTF" 2>"$ERRF"
    RC=$?
  else
    "$BUZZ_BIN" channels list >"$OUTF" 2>"$ERRF"
    RC=$?
  fi
else
  # No CLI — empty feed is success when the relay is up.
  printf '[]\n' >"$OUTF"
  RC=0
fi
echo "RC $RC"
echo "B64 $(base64 -w0 < "$OUTF" 2>/dev/null || base64 < "$OUTF" | tr -d '\n')"
echo "ERRB64 $(base64 -w0 < "$ERRF" 2>/dev/null || base64 < "$ERRF" | tr -d '\n')"
rm -f "$OUTF" "$ERRF"
`, shSingle(relay), shSingle(opKey), buzzImage, buzzInstallDir, shSingle(chanID), buzzHTTPPort)
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
		// Non-zero RC is fine: stderr is framed; parseBuzzPoll surfaces it.
		_ = err
		return parseBuzzPoll(gen, out)
	}
}

func parseBuzzPoll(gen int, out string) buzzPollMsg {
	msg := buzzPollMsg{gen: gen, identity: buzzOperatorName}
	var b64Out, b64Err, buzzErr string
	var rc int
	relayUp := -1 // -1 unknown, 0 down, 1 up
	sawFrame := false
	for _, ln := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(trim, "CHAN "):
			msg.channel = strings.TrimSpace(strings.TrimPrefix(trim, "CHAN "))
		case strings.HasPrefix(trim, "RELAY_UP "):
			_, _ = fmt.Sscanf(trim, "RELAY_UP %d", &relayUp)
			sawFrame = true
		case strings.HasPrefix(trim, "RC "):
			_, _ = fmt.Sscanf(trim, "RC %d", &rc)
			sawFrame = true
		case strings.HasPrefix(trim, "B64 "):
			b64Out = strings.TrimSpace(strings.TrimPrefix(trim, "B64 "))
			sawFrame = true
		case strings.HasPrefix(trim, "ERRB64 "):
			b64Err = strings.TrimSpace(strings.TrimPrefix(trim, "ERRB64 "))
			sawFrame = true
		case strings.HasPrefix(trim, "BUZZ_ERR "):
			buzzErr = strings.TrimSpace(strings.TrimPrefix(trim, "BUZZ_ERR "))
		}
	}
	if relayUp == 0 {
		msg.err = fmt.Errorf("relay not reachable on gateway :%s — ctrl+d to (re)deploy", buzzHTTPPort)
		return msg
	}
	body := ""
	if b64Out != "" {
		if raw, err := decodeB64(b64Out); err == nil {
			body = strings.TrimSpace(string(raw))
		}
	}
	if b64Err != "" {
		if raw, err := decodeB64(b64Err); err == nil {
			buzzErr = strings.TrimSpace(string(raw))
		}
	}
	// Legacy fallbacks (pre-base64 poll script / partial output).
	if body == "" && !sawFrame {
		body = extractJSONBetweenMarkers(out)
		if body == "" {
			body = extractJSONPayload(out)
		}
	}
	if body == "" {
		body = "[]"
	}
	// Strip ANSI / CR so pretty terminals don't poison Unmarshal.
	body = stripANSI(strings.ReplaceAll(body, "\r", ""))

	raw, err := decodeBuzzJSON(body)
	if err != nil {
		// Prefer CLI error JSON when stdout was unusable.
		if buzzErr != "" {
			if m := buzzErrorMessage(buzzErr); m != "" {
				msg.err = fmt.Errorf("buzz-cli: %s", m)
				return msg
			}
			msg.err = fmt.Errorf("buzz-cli: %s", truncate(strings.ReplaceAll(buzzErr, "\n", " "), 160))
			return msg
		}
		// Soft-fail: empty feed + diagnostic line rather than blocking the tab.
		// Still surface a short notice so deploy problems are visible.
		msg.lines = []buzzLine{{sys: true, text: "could not parse feed (" + truncate(strings.ReplaceAll(body, "\n", " "), 80) + ")"}}
		if rc != 0 {
			msg.err = fmt.Errorf("buzz-cli exit %d (unreadable output) — ctrl+d to redeploy Buzz", rc)
		} else {
			msg.err = fmt.Errorf("could not parse buzz-cli JSON — ctrl+d to redeploy Buzz")
		}
		return msg
	}
	if obj, ok := raw.(map[string]any); ok {
		if m := buzzStr(obj, "message"); m != "" && buzzStr(obj, "error") != "" {
			msg.err = fmt.Errorf("buzz-cli: %s", m)
			return msg
		}
	}
	// Non-zero RC with empty/usable body: surface stderr if present.
	if rc != 0 && buzzErr != "" && len(buzzJSONArray(raw)) == 0 {
		if m := buzzErrorMessage(buzzErr); m != "" {
			msg.err = fmt.Errorf("buzz-cli: %s", m)
			return msg
		}
	}

	arr := buzzJSONArray(raw)
	if arr == nil {
		if obj, ok := raw.(map[string]any); ok {
			arr = []any{obj}
		}
	}
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
			if n := buzzStr(obj, "name"); n != "" {
				msg.peers = append(msg.peers, n)
			}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(text), "{") && from != "" && !strings.Contains(text, " ") {
			peers[from] = struct{}{}
			continue
		}
		if from != "" {
			peers[from] = struct{}{}
		}
		disp := from
		if len(disp) == 64 && isHex64(disp) {
			disp = disp[:8] + "…"
		}
		msg.lines = append(msg.lines, buzzLine{from: orDefault(disp, "?"), text: text, ts: ts})
	}
	for p := range peers {
		disp := p
		if len(disp) == 64 && isHex64(disp) {
			disp = disp[:8] + "…"
		}
		msg.peers = append(msg.peers, disp)
	}
	return msg
}

func decodeB64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty")
	}
	// Accept std and raw encodings.
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

func buzzErrorMessage(errBody string) string {
	payload := extractJSONPayload(errBody)
	if payload == "" {
		payload = errBody
	}
	raw, err := decodeBuzzJSON(payload)
	if err != nil {
		return ""
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	return buzzStr(obj, "message", "error", "detail")
}

func extractJSONBetweenMarkers(out string) string {
	const begin, end = "JSON_BEGIN", "JSON_END"
	lines := strings.Split(out, "\n")
	var parts []string
	in := false
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		if trim == begin {
			in = true
			parts = parts[:0]
			continue
		}
		if trim == end {
			break
		}
		if in {
			parts = append(parts, ln)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func stripANSI(s string) string {
	// Minimal CSI sequence stripper: ESC [ ... letter
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func decodeBuzzJSON(body string) (any, error) {
	body = strings.TrimSpace(body)
	if body == "" || body == "null" {
		return []any{}, nil
	}
	var raw any
	if err := json.Unmarshal([]byte(body), &raw); err == nil {
		return raw, nil
	}
	// Retry after extracting embedded JSON (banners/warnings before the payload).
	if payload := extractJSONPayload(body); payload != "" && payload != body {
		if err := json.Unmarshal([]byte(payload), &raw); err == nil {
			return raw, nil
		}
		body = payload
	}
	// NDJSON: one object per line.
	var arr []any
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var item any
		if json.Unmarshal([]byte(ln), &item) != nil {
			return nil, fmt.Errorf("invalid json")
		}
		arr = append(arr, item)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("invalid json")
	}
	return arr, nil
}

// extractJSONPayload returns the first JSON array/object substring in s.
func extractJSONPayload(s string) string {
	s = strings.TrimSpace(s)
	iArr := strings.IndexByte(s, '[')
	iObj := strings.IndexByte(s, '{')
	start := -1
	endByte := byte(0)
	switch {
	case iArr >= 0 && (iObj < 0 || iArr < iObj):
		start, endByte = iArr, ']'
	case iObj >= 0:
		start, endByte = iObj, '}'
	default:
		return ""
	}
	rest := s[start:]
	if i := strings.LastIndexByte(rest, endByte); i >= 0 {
		return rest[:i+1]
	}
	return rest
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func buzzJSONArray(raw any) []any {
	switch v := raw.(type) {
	case []any:
		return v
	case map[string]any:
		for _, key := range []string{"data", "messages", "items", "channels", "events", "results"} {
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
		// Content is base64-passed to avoid shell quoting breakage.
		contentB64 := base64.StdEncoding.EncodeToString([]byte(text))
		script := fmt.Sprintf(`export PATH="/usr/local/bin:$PATH"
export BUZZ_RELAY_URL='%s' BUZZ_PRIVATE_KEY='%s'
IMG='%s'
CONTENT=$(printf '%%s' '%s' | base64 -d)
buzz_run() {
  if [ -x /usr/local/bin/buzz ]; then /usr/local/bin/buzz "$@"; return $?; fi
  if command -v buzz >/dev/null 2>&1; then buzz "$@"; return $?; fi
  sudo docker run --rm --network host -e BUZZ_RELAY_URL -e BUZZ_PRIVATE_KEY \
    --entrypoint buzz "$IMG" "$@"
}
buzz_run messages send --channel '%s' --content "$CONTENT"
`, shSingle(relay), shSingle(opKey), buzzImage, contentB64, shSingle(chanID))
		var out string
		var err error
		if local {
			raw, e := exec.Command("bash", "-c", script).CombinedOutput()
			out, err = string(raw), e
		} else {
			client, e := dialSSH(host, user, pass)
			if e != nil {
				return notifyMsg("buzz send failed: " + e.Error())
			}
			defer client.Close()
			out, err = runSSH(client, script)
		}
		if err != nil {
			return notifyMsg("buzz send failed: " + truncate(lastNonEmptyLine(out)+" "+err.Error(), 160))
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
