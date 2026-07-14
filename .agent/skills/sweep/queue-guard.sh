#!/bin/bash
# Claude activates this hook only while /sweep is active. Queue state is authoritative on every
# Stop attempt: the model can release itself by finishing or blocking each actionable task.

queue_paths() {
  if command -v coop >/dev/null 2>&1; then
    paths=$(cd "$CLAUDE_PROJECT_DIR" && coop tasks queues 2>/dev/null)
    if [ -n "$paths" ]; then
      printf '%s\n' "$paths"
      return
    fi
  fi
  find "$CLAUDE_PROJECT_DIR" -type d -path '*/.agent/tasks' -prune -print 2>/dev/null
}

left=0
while IFS= read -r q; do
  for state in 00_todo 10_in_progress; do
    [ -d "$q/$state" ] || continue
    n=$(find "$q/$state" -mindepth 2 -maxdepth 2 -name task.md 2>/dev/null | wc -l | tr -d ' ')
    left=$((left + ${n:-0}))
  done
done < <(queue_paths)
if [ "${left:-0}" -gt 0 ]; then
  echo "$left actionable task(s) remain in 00_todo or 10_in_progress across the repo's queues. Keep sweeping: finish or block every task before stopping." >&2
  exit 2
fi
