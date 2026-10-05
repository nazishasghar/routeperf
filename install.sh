#!/bin/sh
# routeperf installer
#   curl -fsSL https://raw.githubusercontent.com/nazishasghar/routeperf/main/install.sh | sh
# Options (env vars):
#   ROUTEPERF_VERSION=v0.1.0      install a specific release (default: latest)
#   ROUTEPERF_INSTALL_DIR=/path   where to put the binary (default: /usr/local/bin if writable, else ~/.local/bin)
#   ROUTEPERF_BIN=perfcheck       command name to install as (default: routeperf)
#   ROUTEPERF_FROM_SOURCE=1       build from the current checkout with Go instead of downloading
#   ROUTEPERF_BASE_URL=https://…  download archives from your own server/mirror instead of GitHub releases
set -eu

REPO="${ROUTEPERF_REPO:-nazishasghar/routeperf}"
VERSION="${ROUTEPERF_VERSION:-latest}"
BIN="${ROUTEPERF_BIN:-routeperf}"

say() { printf '%s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }

if [ -z "${ROUTEPERF_INSTALL_DIR:-}" ]; then
  if [ -w /usr/local/bin ]; then DIR=/usr/local/bin; else DIR="$HOME/.local/bin"; fi
else
  DIR="$ROUTEPERF_INSTALL_DIR"
fi
mkdir -p "$DIR"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in darwin|linux) ;; *) die "unsupported OS $os (Windows: download the .zip from the releases page)";; esac
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; *) die "unsupported CPU $arch";; esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

from_source() {
  command -v go >/dev/null 2>&1 || die "Go is not installed (https://go.dev/dl) and no release could be downloaded"
  [ -f go.mod ] && [ -d cmd/routeperf ] || die "run ROUTEPERF_FROM_SOURCE=1 ./install.sh from a routeperf checkout"
  say "building from source with $(go version | cut -d' ' -f3)…"
  mod=$(go list -m)
  v=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X $mod/internal/version.Version=$v" -o "$tmp/routeperf" ./cmd/routeperf
}

download() {
  name="routeperf_${os}_${arch}.tar.gz"
  if [ -n "${ROUTEPERF_BASE_URL:-}" ]; then base="${ROUTEPERF_BASE_URL%/}"
  elif [ "$VERSION" = latest ]; then base="https://github.com/$REPO/releases/latest/download"
  else base="https://github.com/$REPO/releases/download/$VERSION"; fi
  say "downloading $base/$name"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$base/$name" -o "$tmp/$name" || return 1
    curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null || true
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$tmp/$name" "$base/$name" || return 1
    wget -qO "$tmp/checksums.txt" "$base/checksums.txt" 2>/dev/null || true
  else die "need curl or wget"; fi
  if [ -s "$tmp/checksums.txt" ]; then
    want=$(grep " $name\$" "$tmp/checksums.txt" | cut -d' ' -f1)
    if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$name" | cut -d' ' -f1)
    else got=$(shasum -a 256 "$tmp/$name" | cut -d' ' -f1); fi
    [ -z "$want" ] || [ "$want" = "$got" ] || die "checksum mismatch for $name"
  fi
  tar -xzf "$tmp/$name" -C "$tmp"
  mv "$tmp/routeperf_${os}_${arch}/routeperf" "$tmp/routeperf"
}

if [ "${ROUTEPERF_FROM_SOURCE:-0}" = 1 ]; then from_source
elif ! download; then say "download failed; trying a source build"; from_source; fi

install -m 0755 "$tmp/routeperf" "$DIR/$BIN"
say "installed $DIR/$BIN ($("$DIR/$BIN" version))"
case ":$PATH:" in *":$DIR:"*) ;; *)
  say ""
  say "$DIR is not on your PATH. Add it:"
  say "  echo 'export PATH=\"$DIR:\$PATH\"' >> ~/.zshrc   # or ~/.bashrc"
;; esac
say ""
say "Next:  $BIN init   →   $BIN check   →   $BIN run"
