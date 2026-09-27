#!/usr/bin/env bash
# toolyard one-line installer — prebuilt binary, laptop/quick-start flavor.
#
#   curl -fsSL https://raw.githubusercontent.com/iamtushar324/toolyard/main/install.sh | bash
#
# What it does:
#   1. detects your OS/arch (darwin|linux, amd64|arm64)
#   2. downloads the matching tarball from the latest GitHub Release
#   3. verifies it against the release's SHA256SUMS
#   4. installs the `toolyard` binary to /usr/local/bin (or ~/.local/bin)
#
# For a production Linux/systemd install that builds from source and sets
# up the full stack (systemd unit, ClickHouse), use
# deploy/install.sh from a repo checkout instead.
#
# Environment:
#   TOOLYARD_VERSION=v0.1.0   install a specific release instead of latest
#   TOOLYARD_INSTALL_DIR=...  override the install directory

set -euo pipefail

REPO="iamtushar324/toolyard"
INSTALL_DIR="${TOOLYARD_INSTALL_DIR:-/usr/local/bin}"

say()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ----- platform detection ----------------------------------------------
case "$(uname -s)" in
  Darwin) OS="darwin" ;;
  Linux)  OS="linux" ;;
  *) fail "unsupported OS: $(uname -s). Build from source: https://github.com/$REPO#build-from-source" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) fail "unsupported architecture: $(uname -m). Build from source: https://github.com/$REPO#build-from-source" ;;
esac

# ----- resolve version --------------------------------------------------
TAG="${TOOLYARD_VERSION:-}"
if [[ -n "$TAG" && "$TAG" != v* ]]; then
  TAG="v$TAG"
fi
if [[ -z "$TAG" ]]; then
  say "resolving latest release"
  TAG="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')" || true
  [[ -n "$TAG" ]] || fail "could not resolve the latest release (rate-limited, or no releases yet). Set TOOLYARD_VERSION=vX.Y.Z and retry."
fi

TARBALL="toolyard_${TAG}_${OS}_${ARCH}.tar.gz"
BASE_URL="https://github.com/$REPO/releases/download/$TAG"

# ----- download + verify -------------------------------------------------
TMPDIR_DL="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_DL"' EXIT

say "downloading $TARBALL ($TAG)"
curl -fsSL -o "$TMPDIR_DL/$TARBALL" "$BASE_URL/$TARBALL" \
  || fail "download failed: $BASE_URL/$TARBALL (no build for ${OS}/${ARCH} in $TAG?)"
curl -fsSL -o "$TMPDIR_DL/SHA256SUMS" "$BASE_URL/SHA256SUMS" \
  || fail "download failed: $BASE_URL/SHA256SUMS"

say "verifying checksum"
if command -v sha256sum >/dev/null 2>&1; then
  SHACMD="sha256sum"
else
  SHACMD="shasum -a 256"
fi
(
  cd "$TMPDIR_DL"
  grep " $TARBALL\$" SHA256SUMS | $SHACMD -c - >/dev/null
) || fail "checksum mismatch for $TARBALL — refusing to install. Re-run, and if it persists open an issue."

say "extracting"
tar -xzf "$TMPDIR_DL/$TARBALL" -C "$TMPDIR_DL" toolyard

# ----- install -----------------------------------------------------------
install_to() {
  install -m 0755 "$TMPDIR_DL/toolyard" "$1/toolyard"
}

if [[ -w "$INSTALL_DIR" ]]; then
  say "installing -> $INSTALL_DIR/toolyard"
  install_to "$INSTALL_DIR"
elif command -v sudo >/dev/null 2>&1 && [[ -t 1 || -t 0 ]]; then
  say "installing -> $INSTALL_DIR/toolyard (needs sudo)"
  sudo install -m 0755 "$TMPDIR_DL/toolyard" "$INSTALL_DIR/toolyard"
else
  INSTALL_DIR="$HOME/.local/bin"
  say "no write access to /usr/local/bin; installing -> $INSTALL_DIR/toolyard"
  mkdir -p "$INSTALL_DIR"
  install_to "$INSTALL_DIR"
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) printf '\033[1;33mnote:\033[0m %s is not on your PATH — add it to your shell profile.\n' "$INSTALL_DIR" ;;
  esac
fi

# macOS: curl|tar downloads don't normally carry the quarantine xattr, but
# clear it defensively (covers a browser-downloaded copy of this script's
# artifacts). The binary is unsigned; Gatekeeper only checks app bundles.
if [[ "$OS" == "darwin" ]]; then
  xattr -d com.apple.quarantine "$INSTALL_DIR/toolyard" 2>/dev/null || true
fi

INSTALLED_VERSION="$("$INSTALL_DIR/toolyard" version 2>/dev/null || echo "?")"

cat <<EOF

────────────────────────────────────────────────────────────────────────
toolyard installed: $INSTALL_DIR/toolyard ($INSTALLED_VERSION)

  Start the gateway:
      toolyard serve -addr :8787 -data ~/.toolyard

  Then open http://localhost:8787 — create your admin account, enroll
  your first agent (Claude Code, Codex, Cursor, ...), and add upstreams.

  Production Linux/systemd install (full stack):
      git clone https://github.com/$REPO && cd toolyard
      sudo ./deploy/install.sh
────────────────────────────────────────────────────────────────────────
EOF
