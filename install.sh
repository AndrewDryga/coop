#!/usr/bin/env sh
# Install the prebuilt `coop` binary — no Go, no clone:
#   curl -fsSL https://raw.githubusercontent.com/AndrewDryga/coop/main/install.sh | sh
# Env: COOP_VERSION (pin a release tag), COOP_BIN_DIR (default ~/.local/bin),
#      COOP_NO_BUILD=1 (skip building the box image).
set -eu

repo="AndrewDryga/coop"
bindir="${COOP_BIN_DIR:-$HOME/.local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "coop: unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  linux | darwin) ;;
  *) echo "coop: unsupported OS: $os (Linux and macOS only)" >&2; exit 1 ;;
esac

ver="${COOP_VERSION:-}"
if [ -z "$ver" ]; then
  ver=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" |
    grep '"tag_name"' | head -1 | cut -d '"' -f 4)
fi
[ -n "$ver" ] || { echo "coop: could not resolve the latest release; set COOP_VERSION" >&2; exit 1; }

asset="coop_${ver#v}_${os}_${arch}.tar.gz"
url="https://github.com/$repo/releases/download/$ver/$asset"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "coop: downloading $asset ($ver)…"
curl -fsSL "$url" -o "$tmp/coop.tar.gz" || { echo "coop: download failed: $url" >&2; exit 1; }
tar -xzf "$tmp/coop.tar.gz" -C "$tmp"
mkdir -p "$bindir"
install -m 0755 "$tmp/coop" "$bindir/coop"
echo "coop: installed $bindir/coop ($("$bindir/coop" version))"

case ":$PATH:" in
  *":$bindir:"*) ;;
  *) printf '\n  %s is not on your PATH — add to your shell rc:\n    export PATH="%s:$PATH"\n\n' "$bindir" "$bindir" ;;
esac

# Carry over an existing agent-box config (the pre-rename location), once.
conf="${XDG_CONFIG_HOME:-$HOME/.config}/coop"
old="${XDG_CONFIG_HOME:-$HOME/.config}/agent-box/agents"
if [ ! -d "$conf/agents" ] && [ -d "$old" ]; then
  mkdir -p "$conf"
  cp -R "$old" "$conf/agents"
  echo "coop: migrated config from $old"
fi

# Build the sandbox image + verify, when a container runtime is available.
if [ "${COOP_NO_BUILD:-0}" = 1 ]; then
  echo "coop: skipped image build (COOP_NO_BUILD=1) — next: coop build && coop doctor"
elif command -v container >/dev/null 2>&1 || command -v docker >/dev/null 2>&1 || command -v podman >/dev/null 2>&1; then
  "$bindir/coop" build && "$bindir/coop" doctor
else
  echo "coop: no container runtime found — install Docker, Podman, or Apple 'container',"
  echo "      then run: coop build && coop doctor"
fi

echo
echo "Done. From any repo:  coop        # sandboxed claude"
