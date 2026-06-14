#!/bin/bash
# Fast commit gate: format staged files, block the commit if they're dirty.
# Reads the tool call on stdin; only acts on `git commit`. Fails open.
input=$(cat)
echo "$input" | grep -q '"command"[^}]*git commit' || exit 0
staged=$(git diff --cached --name-only --diff-filter=ACM 2>/dev/null) || exit 0
# Go
go_files=$(echo "$staged" | grep '\.go$' || true)
[ -n "$go_files" ] && command -v gofmt >/dev/null && {
  bad=$(gofmt -l $go_files 2>/dev/null)
  [ -n "$bad" ] && { echo "gofmt: $bad" >&2; exit 2; }
}
# Add your own fast checks here (e.g. mix format --check-formatted).
exit 0
