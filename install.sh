#!/bin/sh
# pgpool installer. Downloads the latest release of pgpool and pgpoolcli for
# the current OS/arch and installs them into $INSTALL_DIR (default /usr/local/bin).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/sythelabs/pgpool/main/install.sh | sh
#
# Env vars:
#   PGPOOL_VERSION   Tag to install (default: latest release)
#   INSTALL_DIR      Install destination (default: /usr/local/bin)

set -eu

REPO="sythelabs/pgpool"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
VERSION="${PGPOOL_VERSION:-}"

err() { echo "pgpool-install: $*" >&2; exit 1; }
info() { echo "pgpool-install: $*"; }

have() { command -v "$1" >/dev/null 2>&1; }

have curl || err "curl is required"
have tar  || err "tar is required"

uname_s="$(uname -s)"
uname_m="$(uname -m)"

case "$uname_s" in
  Linux)  os="linux" ;;
  Darwin) os="darwin" ;;
  *) err "unsupported OS: $uname_s (Windows users: download the .zip from https://github.com/$REPO/releases)" ;;
esac

case "$uname_m" in
  x86_64|amd64)  arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) err "unsupported arch: $uname_m" ;;
esac

if [ -z "$VERSION" ]; then
  info "resolving latest release"
  VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
  [ -n "$VERSION" ] || err "failed to resolve latest release tag"
fi

asset="pgpool-${VERSION}-${os}-${arch}.tar.gz"
url="https://github.com/$REPO/releases/download/${VERSION}/${asset}"

tmpdir="$(mktemp -d 2>/dev/null || mktemp -d -t pgpool)"
trap 'rm -rf "$tmpdir"' EXIT

info "downloading $asset"
curl -fsSL -o "$tmpdir/$asset" "$url" \
  || err "download failed: $url"

info "extracting"
tar -xzf "$tmpdir/$asset" -C "$tmpdir"

extracted="$tmpdir/pgpool-${VERSION}-${os}-${arch}"
[ -x "$extracted/pgpool" ]    || err "pgpool binary missing from archive"
[ -x "$extracted/pgpoolcli" ] || err "pgpoolcli binary missing from archive"

sudo_cmd=""
if [ ! -w "$INSTALL_DIR" ]; then
  if have sudo; then
    sudo_cmd="sudo"
    info "$INSTALL_DIR not writable - will use sudo"
  else
    err "$INSTALL_DIR not writable and sudo not found; set INSTALL_DIR to a writable path"
  fi
fi

$sudo_cmd install -m 0755 "$extracted/pgpool"    "$INSTALL_DIR/pgpool"
$sudo_cmd install -m 0755 "$extracted/pgpoolcli" "$INSTALL_DIR/pgpoolcli"

info "installed pgpool and pgpoolcli ($VERSION) to $INSTALL_DIR"
info "next: run 'pgpoolcli init' on each client machine"
