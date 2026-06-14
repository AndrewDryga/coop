#!/usr/bin/env bash
# Build the `coop` binary, install it onto your PATH, build the box image, and
# verify the sandbox holds. Re-runnable; safe to run after every git pull.
set -euo pipefail

src="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bindir="${COOP_BIN_DIR:-$HOME/.local/bin}"
confdir="${XDG_CONFIG_HOME:-$HOME/.config}/coop"

command -v go >/dev/null 2>&1 || {
  echo "coop needs Go to build — install it from https://go.dev/dl/ and retry" >&2
  exit 1
}

echo "building coop (go build)…"
mkdir -p "$bindir"
( cd "$src" && go build -trimpath -o "$bindir/coop" . )
echo "installed $bindir/coop"

case ":$PATH:" in
  *":$bindir:"*) ;;
  *) echo; echo "  ⚠  $bindir is not on your PATH. Add this to your shell rc:";
     echo "       export PATH=\"$bindir:\$PATH\""; echo ;;
esac

# One-time migration: seed the XDG config dir so an existing login carries over.
# Prefer the previous agent-box location, then an in-repo agents/ folder.
if [ ! -d "$confdir/agents" ]; then
  for srcdir in "${XDG_CONFIG_HOME:-$HOME/.config}/agent-box/agents" "$src/agents"; do
    if [ -d "$srcdir" ]; then
      mkdir -p "$confdir"
      cp -R "$srcdir" "$confdir/agents"
      echo "seeded config -> $confdir/agents (from $srcdir)"
      break
    fi
  done
fi

"$bindir/coop" build
"$bindir/coop" doctor

echo
echo "Done. From any repo:  coop        # sandboxed claude"
echo "                      coop doctor # re-verify the box anytime"
