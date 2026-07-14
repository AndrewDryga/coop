#!/bin/bash
# Claude activates this hook only while /sweep is active. Queue state is authoritative on every
# Stop attempt: the model can release itself by finishing or blocking each actionable task.

queue_paths() {
  if command -v coop >/dev/null 2>&1; then
    paths=$(cd "$CLAUDE_PROJECT_DIR" && coop tasks queues) || return 1
    printf '%s\n' "$paths"
    return
  fi
  find "$CLAUDE_PROJECT_DIR" -type d -path '*/.agent/tasks' -prune -print
}

paths=$(queue_paths) || {
  echo "Sweep queue guard could not discover task queues; refusing to stop. Fix the reported error and retry." >&2
  exit 2
}
left=0
while IFS= read -r q; do
  [ -n "$q" ] || continue
  if [ -L "$q" ]; then
    echo "Sweep queue guard refuses symlinked queue root: $q" >&2
    exit 2
  fi
  if [ ! -d "$q" ]; then
    echo "Sweep queue guard cannot read configured queue root: $q" >&2
    exit 2
  fi
  for state in 00_todo 10_in_progress; do
    [ -d "$q/$state" ] || continue
    tasks=$(find "$q/$state" -mindepth 1 -maxdepth 1 -type d -print) || {
      echo "Sweep queue guard cannot count $q/$state; refusing to stop." >&2
      exit 2
    }
    n=0
    while IFS= read -r task; do
      [ -n "$task" ] && n=$((n + 1))
    done <<< "$tasks"
    left=$((left + ${n:-0}))
  done
done <<< "$paths"
if [ "${left:-0}" -gt 0 ]; then
  echo "$left actionable task(s) remain in 00_todo or 10_in_progress across the repo's queues. Keep sweeping: finish or block every task before stopping." >&2
  exit 2
fi
