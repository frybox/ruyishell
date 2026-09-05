#!/usr/bin/env bash
# install.sh - install ruyishell (rysh) from a prebuilt release archive or
# from source.
#
# Prebuilt binary (default; needs curl + tar, unzip for Windows):
#   curl -fsSL https://github.com/frybox/ruyishell/releases/latest/download/install.sh | bash
#   # or, from a checkout:
#   RYSH_RELEASE_URL=https://host/releases/v0.1.0 ./scripts/install.sh
# RYSH_RELEASE_URL points at the directory holding the release archives
# (rysh-<os>-<arch>.tar.gz / .zip + checksums.txt, built by `make package`).
# When this script is piped in (curl ... | bash) it falls back to RELEASE_BASE
# below — if you fork, point it at your fork's account.
#
# From source (needs Go 1.25+):
#   ./scripts/install.sh --source            # builds from this checkout
#   PREFIX=/usr/local ./scripts/install.sh --source
#
# After install, ensure the install prefix is on your $PATH. On Windows, the
# recommended path is to download the release zip, extract it, and put rysh.exe
# on your PATH — this script targets Unix.

set -euo pipefail

PREFIX="${PREFIX:-$HOME/.local/bin}"
BIN_NAME="rysh"
REPO_PKG="./cmd/rysh"
SOURCE_ONLY=0
[ "${1:-}" = "--source" ] && SOURCE_ONLY=1

# Default asset directory for a piped install (curl ... | bash), where the
# script has no other way to learn where the archives live.
RELEASE_BASE="https://github.com/frybox/ruyishell/releases/latest/download"

err() { printf 'install: %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || err "required command not found: $1"; }

# Global temp dir for the download/extract step; cleaned on exit.
TMPDIR_CLEAN=""
trap 'rm -rf "$TMPDIR_CLEAN"' EXIT

detect() {
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    i386 | i686) arch=386 ;;
  esac
}

install_from_source() {
  need go
  local v="${1:-$(git describe --tags --always 2>/dev/null || echo dev)}"
  printf 'install: building from source (version %s)\n' "$v"
  mkdir -p "$PREFIX"
  CGO_ENABLED=0 go install -trimpath -ldflags "-X main.version=$v" "$REPO_PKG"
  local gobin
  gobin="$(GOFLAGS= go env GOBIN || true)"
  [ -z "$gobin" ] && gobin="$(go env GOPATH)/bin"
  local src="$gobin/$BIN_NAME"
  [ -f "$src" ] || err "go install did not produce $src"
  install -m 0755 "$src" "$PREFIX/$BIN_NAME"
  printf 'install: installed %s -> %s/%s\n' "$v" "$PREFIX" "$BIN_NAME"
}

install_from_url() {
  local url="$1"
  need curl; need tar
  detect
  local base
  base="${url%/}"
  local file bin="$BIN_NAME-$os-$arch"
  if [ "$os" = "windows" ]; then
    file="$bin.zip"; need unzip
  else
    file="$bin.tar.gz"
  fi
  local tmp
  tmp="$(mktemp -d)"
  TMPDIR_CLEAN="$tmp"
  printf 'install: downloading %s\n' "$base/$file"
  curl -fsSL "$base/$file" -o "$tmp/$file"
  # Extract the whole archive (binary + checksums.txt).
  case "$file" in
  *.tar.gz) tar -xzf "$tmp/$file" -C "$tmp" ;;
  *.zip) unzip -q "$tmp/$file" -d "$tmp" ;;
  esac
  # Verify the binary against the shipped checksums.txt, when present.
  if [ -f "$tmp/checksums.txt" ]; then
    need sha256sum
    ( cd "$tmp" && grep -F "$bin" checksums.txt | sha256sum -c - )
  else
    printf 'install: warning: no checksums.txt in archive, skipping verification\n'
  fi
  [ -f "$tmp/$bin" ] || err "archive did not contain $bin"
  mkdir -p "$PREFIX"
  install -m 0755 "$tmp/$bin" "$PREFIX/$BIN_NAME"
  printf 'install: installed %s -> %s/%s\n' "$os/$arch" "$PREFIX" "$BIN_NAME"
}

main() {
  if [ "$SOURCE_ONLY" = 1 ]; then
    install_from_source
  elif [ -n "${RYSH_RELEASE_URL:-}" ]; then
    install_from_url "$RYSH_RELEASE_URL"
  elif [ -n "${BASH_SOURCE:-}" ] && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    # No release URL set and we are running from a checkout: build from source.
    install_from_source
  else
    # Piped install (curl ... | bash): use the default GitHub release base.
    install_from_url "$RELEASE_BASE"
  fi
  printf '\ninstall: done. Ensure %s is on your $PATH.\n' "$PREFIX"
  "$PREFIX/$BIN_NAME" --version 2>/dev/null || true
}

main "$@"
