#!/usr/bin/env sh
# One-line installer for the oilsand-ai-gateway TUI (Linux/macOS).
#
#   curl -fsSL https://raw.githubusercontent.com/script-repo/oilsand-ai-gateway/main/scripts/install.sh | sh
#
# Downloads the latest GitHub release archive for your OS/arch and extracts the
# binary together with the bundled scripts/ helpers (so Nutanix deploy works).
# Override the version with OILSAND_VERSION=v1.2.3 and the location with
# OILSAND_INSTALL_DIR=/some/dir.
set -eu

REPO="script-repo/oilsand-ai-gateway"
BIN_NAME="oilsand-tui"
INSTALL_DIR="${OILSAND_INSTALL_DIR:-$HOME/.oilsand-ai-gateway}"

info() { printf '[install] %s\n' "$*"; }
fail() { printf '[install] ERROR: %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || fail "required tool '$1' not found"; }
need uname
need tar

# Pick a downloader.
if command -v curl >/dev/null 2>&1; then
  dl() { curl -fsSL "$1" -o "$2"; }
  fetch() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
  dl() { wget -qO "$2" "$1"; }
  fetch() { wget -qO - "$1"; }
else
  fail "need curl or wget"
fi

# Map uname -> GoReleaser os/arch.
os="$(uname -s)"
case "$os" in
  Linux) OS="linux" ;;
  Darwin) OS="darwin" ;;
  *) fail "unsupported OS: $os (use the Windows install.ps1)" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64) ARCH="amd64" ;;
  arm64 | aarch64) ARCH="arm64" ;;
  *) fail "unsupported arch: $arch" ;;
esac

# Resolve version (latest unless pinned).
VERSION="${OILSAND_VERSION:-}"
if [ -z "$VERSION" ]; then
  info "resolving latest release"
  VERSION="$(fetch "https://api.github.com/repos/$REPO/releases/latest" \
    | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' \
    | head -n1 | sed 's/.*"\([^"]*\)"$/\1/')"
  [ -n "$VERSION" ] || fail "could not determine latest release; set OILSAND_VERSION"
fi
VER_NO_V="${VERSION#v}"

ASSET="${BIN_NAME}_${VER_NO_V}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

info "downloading $ASSET ($VERSION)"
dl "$URL" "$tmp/$ASSET" || fail "download failed: $URL"

info "installing to $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"
tar -xzf "$tmp/$ASSET" -C "$INSTALL_DIR"
chmod +x "$INSTALL_DIR/$BIN_NAME" 2>/dev/null || true

# Best-effort: link the binary onto PATH.
BIN_DIR="${OILSAND_BIN_DIR:-$HOME/.local/bin}"
mkdir -p "$BIN_DIR" 2>/dev/null || true
if ln -sf "$INSTALL_DIR/$BIN_NAME" "$BIN_DIR/$BIN_NAME" 2>/dev/null; then
  info "linked $BIN_DIR/$BIN_NAME -> $INSTALL_DIR/$BIN_NAME"
  case ":$PATH:" in
    *":$BIN_DIR:"*) : ;;
    *) info "add $BIN_DIR to your PATH:  export PATH=\"$BIN_DIR:\$PATH\"" ;;
  esac
else
  info "run it directly:  $INSTALL_DIR/$BIN_NAME"
fi

# ---- interactive feature dependencies --------------------------------------
# The Nutanix deploy feature shells out to Python (requests + paramiko); the
# Console/Agents features shell out to the OpenSSH client. Put both in place.
# System-package installs run non-interactively (sudo -n) so a piped installer
# never hangs on a password prompt; if that's not possible we print the command.
# Set OILSAND_SKIP_DEPS=1 to skip this section entirely.
have() { command -v "$1" >/dev/null 2>&1; }

if [ "${OILSAND_SKIP_DEPS:-0}" = "1" ]; then
  info "skipping dependency setup (OILSAND_SKIP_DEPS=1)"
else
  SUDO=""
  if [ "$(id -u)" -ne 0 ] && have sudo; then SUDO="sudo -n"; fi

  PM=""
  for c in apt-get dnf yum zypper pacman apk brew; do
    if have "$c"; then PM="$c"; break; fi
  done

  pm_install() {
    [ -n "$PM" ] || return 1
    case "$PM" in
      apt-get) $SUDO apt-get update -qq >/dev/null 2>&1; $SUDO apt-get install -y "$@" >/dev/null 2>&1 ;;
      dnf)     $SUDO dnf install -y "$@" >/dev/null 2>&1 ;;
      yum)     $SUDO yum install -y "$@" >/dev/null 2>&1 ;;
      zypper)  $SUDO zypper --non-interactive install "$@" >/dev/null 2>&1 ;;
      pacman)  $SUDO pacman -Sy --noconfirm "$@" >/dev/null 2>&1 ;;
      apk)     $SUDO apk add "$@" >/dev/null 2>&1 ;;
      brew)    brew install "$@" >/dev/null 2>&1 ;;
    esac
  }

  case "$PM" in
    apt-get)        PY_PKGS="python3 python3-venv python3-pip"; SSH_PKG="openssh-client" ;;
    dnf|yum|zypper) PY_PKGS="python3 python3-pip";              SSH_PKG="openssh-clients" ;;
    pacman)         PY_PKGS="python";                           SSH_PKG="openssh" ;;
    apk)            PY_PKGS="python3 py3-pip";                  SSH_PKG="openssh-client" ;;
    brew)           PY_PKGS="python";                           SSH_PKG="" ;; # ssh preinstalled on macOS
    *)              PY_PKGS="python3";                          SSH_PKG="openssh-client" ;;
  esac

  # OpenSSH client (Console / Agents).
  if have ssh; then
    info "openssh client: ok"
  elif [ -n "$SSH_PKG" ] && pm_install $SSH_PKG && have ssh; then
    info "openssh client installed"
  else
    info "WARNING: openssh client not available (needed for Console/Agents)."
    [ -n "$SSH_PKG" ] && info "  install it with:  $SUDO $PM install $SSH_PKG"
  fi

  # Python 3 (Nutanix deploy).
  have python3 || pm_install $PY_PKGS || true

  REQ="$INSTALL_DIR/requirements.txt"
  VENV="$INSTALL_DIR/venv"
  if have python3 && [ -f "$REQ" ]; then
    info "setting up Python venv for Nutanix deploy ($VENV)"
    if python3 -m venv "$VENV" >/dev/null 2>&1; then
      "$VENV/bin/python" -m pip install --quiet --upgrade pip >/dev/null 2>&1 || true
      if "$VENV/bin/python" -m pip install --quiet -r "$REQ" >/dev/null 2>&1; then
        info "python deps installed (requests, paramiko); the TUI uses $VENV automatically"
      else
        info "WARNING: pip install failed; run:  $VENV/bin/python -m pip install -r $REQ"
      fi
    elif python3 -m pip install --quiet --user -r "$REQ" >/dev/null 2>&1; then
      info "python deps installed to your user site (venv module unavailable)"
    else
      info "WARNING: could not install python deps automatically."
      info "  install python3-venv (Debian/Ubuntu) and re-run, or:  python3 -m pip install -r $REQ"
    fi
  elif ! have python3; then
    info "WARNING: python3 not found (needed for Nutanix deploy)."
    info "  install Python 3, then:  python3 -m pip install -r $REQ"
  fi
fi

info "done."
