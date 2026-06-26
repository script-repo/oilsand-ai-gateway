#!/usr/bin/env bash
# Native Olla installer for a Rocky Linux VM.
# Installs the Olla binary to /usr/local/bin, writes /etc/olla/olla.yaml,
# and runs Olla as a systemd service listening on 0.0.0.0:${OLLA_PORT}.
#
# Usage: sudo ./install-olla.sh
# Tunables (env): OLLA_PORT (default 40114), OLLA_VERSION (default latest).
set -euo pipefail

OLLA_PORT="${OLLA_PORT:-40114}"
OLLA_VERSION="${OLLA_VERSION:-latest}"
OLLA_BIN="/usr/local/bin/olla"
OLLA_CONF_DIR="/etc/olla"
OLLA_CONF="${OLLA_CONF_DIR}/olla.yaml"

log() { printf '[install-olla] %s\n' "$*"; }
fatal() { printf '[install-olla] ERROR: %s\n' "$*" >&2; exit 1; }

if [ "$(id -u)" -ne 0 ]; then
  fatal "must run as root (use sudo)"
fi

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) goarch="amd64" ;;
  aarch64|arm64) goarch="arm64" ;;
  *) fatal "unsupported architecture: ${arch}" ;;
esac

command -v curl >/dev/null 2>&1 || { log "installing curl"; dnf install -y curl >/dev/null 2>&1 || yum install -y curl >/dev/null 2>&1 || true; }
command -v tar >/dev/null 2>&1 || { log "installing tar"; dnf install -y tar >/dev/null 2>&1 || yum install -y tar >/dev/null 2>&1 || true; }

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

install_from_official_script() {
  # The upstream "olla installation script" downloads the correct asset for this
  # platform into the current directory. We then locate/extract the binary.
  log "running upstream Olla installer (install.sh)"
  ( cd "${workdir}" && bash <(curl -fsSL https://raw.githubusercontent.com/thushan/olla/main/install.sh) ) || return 1
  local found
  found="$(find "${workdir}" -maxdepth 3 -type f -name olla -perm -u+x 2>/dev/null | head -n1 || true)"
  if [ -z "${found}" ]; then
    # The asset may have been left as an archive; extract any tarball and retry.
    local tgz
    tgz="$(find "${workdir}" -maxdepth 2 -type f \( -name '*.tar.gz' -o -name '*.tgz' \) 2>/dev/null | head -n1 || true)"
    if [ -n "${tgz}" ]; then
      tar -xzf "${tgz}" -C "${workdir}"
      found="$(find "${workdir}" -maxdepth 3 -type f -name olla 2>/dev/null | head -n1 || true)"
    fi
  fi
  [ -n "${found}" ] || return 1
  install -m 0755 "${found}" "${OLLA_BIN}"
}

install_from_github_release() {
  log "resolving Olla release (${OLLA_VERSION}) from GitHub"
  local tag
  if [ "${OLLA_VERSION}" = "latest" ]; then
    tag="$(curl -fsSL https://api.github.com/repos/thushan/olla/releases/latest \
      | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)"
  else
    tag="${OLLA_VERSION}"
  fi
  [ -n "${tag}" ] || return 1
  local ver="${tag#v}"
  local base="https://github.com/thushan/olla/releases/download/${tag}"
  local candidates=(
    "${base}/olla_${ver}_linux_${goarch}.tar.gz"
    "${base}/olla_linux_${goarch}.tar.gz"
    "${base}/olla_${ver}_Linux_${goarch}.tar.gz"
    "${base}/olla-linux-${goarch}.tar.gz"
  )
  local url
  for url in "${candidates[@]}"; do
    log "trying ${url}"
    if curl -fsSL "${url}" -o "${workdir}/olla.tar.gz"; then
      tar -xzf "${workdir}/olla.tar.gz" -C "${workdir}"
      local found
      found="$(find "${workdir}" -maxdepth 3 -type f -name olla 2>/dev/null | head -n1 || true)"
      if [ -n "${found}" ]; then
        install -m 0755 "${found}" "${OLLA_BIN}"
        return 0
      fi
    fi
  done
  return 1
}

if ! install_from_official_script; then
  log "upstream installer did not yield a binary; falling back to GitHub release download"
  install_from_github_release || fatal "failed to install Olla binary"
fi

"${OLLA_BIN}" --version >/dev/null 2>&1 || log "warning: 'olla --version' did not succeed (continuing)"
log "installed Olla binary: $("${OLLA_BIN}" --version 2>/dev/null | head -n1 || echo unknown)"

id olla >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin olla 2>/dev/null || \
  useradd --system --no-create-home --shell /sbin/nologin olla 2>/dev/null || true

mkdir -p "${OLLA_CONF_DIR}"
if [ ! -f "${OLLA_CONF}" ]; then
  cat >"${OLLA_CONF}" <<EOF
server:
  host: "0.0.0.0"
  port: ${OLLA_PORT}
  request_logging: true
  # Raise the stock 30s timeouts: CPU-only workers can take minutes to load a
  # model and evaluate long prompts. write_timeout must be 0 for streaming.
  read_timeout: 900s
  write_timeout: 0s
  idle_timeout: 900s

proxy:
  engine: "sherpa"
  # least-connections spreads inference load across all workers; "priority"
  # (the Olla default) would pin every request to the highest-priority endpoint.
  load_balancer: "least-connections"
  connection_timeout: 60s
  response_timeout: 900s
  read_timeout: 900s
  # Olla hardcodes the backend response-header wait to 30s; cold model loads on
  # CPU-only workers exceed that and 502 the first request. Raise it (needs Olla
  # v0.0.17+; this gateway runs newer). See thushan/olla issue #163.
  response_header_timeout: 900s

discovery:
  type: "static"
  model_discovery:
    enabled: true
    # Short interval so models pulled onto workers become routable quickly even
    # without a manual gateway refresh (the TUI also restarts Olla after a
    # download to make new models available immediately).
    interval: 1m
  static:
    endpoints: []

logging:
  level: "info"
  format: "json"
EOF
  log "wrote ${OLLA_CONF}"
else
  log "keeping existing ${OLLA_CONF}"
fi
# Model-unification override so models advertise the exact capability tokens
# Olla's router requires for agentic clients (function_calling, tools, code).
# Without this, Ollama models (which expose no model "type" via /api/tags) never
# match a tools/code request, so Olla logs "No models found with required
# capabilities" and falls back to routing anyway. This file is fully managed by
# the gateway tooling, so it is rewritten on every install.
cat >"${OLLA_CONF_DIR}/models.yaml" <<'MODELSEOF'
version: "1.0"

capabilities:
  type_capabilities:
    llm:
      - text-generation
      - chat
      - completion
      - tools
      - function_calling
      - code
    vlm:
      - text-generation
      - vision
      - multimodal
      - image-understanding
      - tools
      - function_calling
    embeddings:
      - embeddings
      - similarity
      - vector-search
    embedding:
      - embeddings
      - similarity
      - vector-search

  name_patterns:
    - pattern: "(code|coder|codegen|starcoder)"
      capabilities:
        - code-generation
        - programming
        - code-completion
        - code
        - tools
        - function_calling
    - pattern: "(instruct|chat|assistant)"
      capabilities:
        - instruction-following
        - chat
        - tools
        - function_calling
        - code
    - pattern: "(reasoning|think)"
      capabilities:
        - reasoning
        - logic
        - tools
        - function_calling
        - code
    - pattern: "(math|mathstral)"
      capabilities:
        - mathematics
        - problem-solving
    - pattern: "(vision|vlm|llava|bakllava)"
      capabilities:
        - vision
        - multimodal
        - image-understanding
    # Tool/function-calling-capable general model families. Extend as needed.
    - pattern: "(minimax|qwen|qwq|llama|mistral|mixtral|magistral|devstral|ministral|gemma|phi|deepseek|hermes|openhermes|dolphin|command|granite|gpt-oss|gptoss|gpt|smollm|olmo|nemotron|cogito|glm|aya|exaone|falcon|mpt|wizard|vicuna|zephyr|starling|solar|tulu|nous|yi)"
      capabilities:
        - tools
        - function_calling
        - code
        - chat
        - instruction-following

  context_thresholds:
    extended_context: 32000
    long_context: 100000
    ultra_long_context: 1000000
MODELSEOF
log "wrote ${OLLA_CONF_DIR}/models.yaml (capability overrides)"

chown -R olla:olla "${OLLA_CONF_DIR}" 2>/dev/null || true

cat >/etc/systemd/system/olla.service <<EOF
[Unit]
Description=Olla LLM proxy and load balancer
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=olla
Group=olla
# Olla creates a ./logs directory relative to its working dir on startup, so it
# needs a writable home. StateDirectory creates /var/lib/olla owned by olla.
StateDirectory=olla
WorkingDirectory=/var/lib/olla
ExecStart=${OLLA_BIN} --config ${OLLA_CONF}
Environment="OLLA_SERVER_HOST=0.0.0.0"
Environment="OLLA_SERVER_PORT=${OLLA_PORT}"
# Make Olla load ${OLLA_CONF_DIR}/models.yaml (capability overrides). Without an
# explicit dir Olla only checks paths relative to its working directory.
Environment="OLLA_CONFIG_DIR=${OLLA_CONF_DIR}"
Restart=always
RestartSec=3
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable olla
systemctl restart olla

# Open the Olla port if firewalld is running so clients can reach the gateway.
if command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld; then
  log "opening firewall port ${OLLA_PORT}/tcp"
  firewall-cmd --permanent --add-port="${OLLA_PORT}/tcp" >/dev/null 2>&1 || true
  firewall-cmd --reload >/dev/null 2>&1 || true
fi

log "waiting for Olla to listen on :${OLLA_PORT}"
ok=0
for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:${OLLA_PORT}/v1/models" >/dev/null 2>&1 \
     || curl -fsS "http://127.0.0.1:${OLLA_PORT}/internal/health" >/dev/null 2>&1 \
     || curl -fsS "http://127.0.0.1:${OLLA_PORT}/" >/dev/null 2>&1; then
    ok=1; break
  fi
  sleep 2
done

systemctl is-active --quiet olla || fatal "olla.service is not active: $(systemctl is-active olla)"
[ "${ok}" -eq 1 ] || log "warning: Olla service active but HTTP probe did not respond yet"
log "Olla installed and running on 0.0.0.0:${OLLA_PORT}"
