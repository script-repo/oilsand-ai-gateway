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

info "done. Nutanix deploy also needs Python 3 + 'pip install -r requirements.txt' (optional)."
