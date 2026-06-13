#!/usr/bin/env bash
# Install the `agent` command onto your PATH, build the image, and verify it.
set -euo pipefail

src="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bindir="${AGENT_BIN_DIR:-$HOME/.local/bin}"

mkdir -p "$bindir"
ln -sf "$src/bin/agent" "$bindir/agent"
echo "linked $bindir/agent -> $src/bin/agent"

case ":$PATH:" in
  *":$bindir:"*) ;;
  *) echo; echo "  ⚠  $bindir is not on your PATH. Add this to your shell rc:";
     echo "       export PATH=\"$bindir:\$PATH\""; echo ;;
esac

"$bindir/agent" build
"$bindir/agent" doctor

echo
echo "Done. From any repo:  agent        # sandboxed claude"
echo "                      agent doctor # re-verify the box anytime"
