#!/usr/bin/env bash
# Native Ollama installer for a Rocky Linux VM.
# Installs Ollama via the official script, runs it as a systemd service bound to
# 0.0.0.0:11434, and optionally pulls a model.
#
# Usage: sudo ./install-ollama.sh [MODEL]
# Tunables (env): OLLAMA_MODEL (alternative to positional MODEL arg).
set -euo pipefail

MODEL="${1:-${OLLAMA_MODEL:-}}"
OLLAMA_PORT="11434"

log() { printf '[install-ollama] %s\n' "$*"; }
fatal() { printf '[install-ollama] ERROR: %s\n' "$*" >&2; exit 1; }

if [ "$(id -u)" -ne 0 ]; then
  fatal "must run as root (use sudo)"
fi

command -v curl >/dev/null 2>&1 || { log "installing curl"; dnf install -y curl >/dev/null 2>&1 || yum install -y curl >/dev/null 2>&1 || true; }
# The official Ollama installer now ships zstd-compressed artifacts and requires
# the zstd tool to extract them; it is not present on the Rocky generic cloud image.
command -v zstd >/dev/null 2>&1 || { log "installing zstd"; dnf install -y zstd >/dev/null 2>&1 || yum install -y zstd >/dev/null 2>&1 || fatal "could not install zstd (required by the Ollama installer)"; }

log "installing Ollama via official install script"
curl -fsSL https://ollama.com/install.sh | sh

# Bind to all interfaces so Olla on another host can reach this worker, and cap
# the resident model set to one. These VMs are modest (e.g. 12GB RAM); keeping
# two models hot at once (an 8B + a 3B with KV cache) OOM-kills ollama. With
# MAX_LOADED_MODELS=1 ollama evicts before loading another, and KEEP_ALIVE keeps
# the active model warm so requests don't pay a cold load.
mkdir -p /etc/systemd/system/ollama.service.d
cat >/etc/systemd/system/ollama.service.d/override.conf <<EOF
[Service]
Environment="OLLAMA_HOST=0.0.0.0:${OLLAMA_PORT}"
Environment="OLLAMA_MAX_LOADED_MODELS=1"
Environment="OLLAMA_KEEP_ALIVE=24h"
Environment="OLLAMA_NUM_PARALLEL=1"
EOF

systemctl daemon-reload
systemctl enable ollama
# The official installer already started ollama (bound to 127.0.0.1); restart so the
# OLLAMA_HOST=0.0.0.0 override takes effect and remote hosts (Olla) can reach it.
systemctl restart ollama

# Open the Ollama port if firewalld is running so the Olla gateway can reach this worker.
if command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld; then
  log "opening firewall port ${OLLAMA_PORT}/tcp"
  firewall-cmd --permanent --add-port="${OLLAMA_PORT}/tcp" >/dev/null 2>&1 || true
  firewall-cmd --reload >/dev/null 2>&1 || true
fi

log "waiting for Ollama API on :${OLLAMA_PORT}"
ready=0
for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:${OLLAMA_PORT}/api/tags" >/dev/null 2>&1; then
    ready=1; break
  fi
  sleep 2
done

systemctl is-active --quiet ollama || fatal "ollama.service is not active: $(systemctl is-active ollama)"
[ "${ready}" -eq 1 ] || fatal "Ollama API did not become ready on :${OLLAMA_PORT}"
log "Ollama running on 0.0.0.0:${OLLAMA_PORT}"

if [ -n "${MODEL}" ]; then
  log "pulling model: ${MODEL}"
  if ollama pull "${MODEL}"; then
    log "model pulled: ${MODEL}"
  else
    fatal "failed to pull model '${MODEL}' (check that the name exists in the Ollama registry)"
  fi
else
  log "no model requested; skipping pull"
fi
