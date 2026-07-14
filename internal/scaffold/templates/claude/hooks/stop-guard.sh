#!/bin/bash
# Refuse to stop while the queue has unclaimed work. Armed only during a batch
# (when .agent/active exists), so it never nags during interactive use.
[ -f "$CLAUDE_PROJECT_DIR/.agent/active" ] || exit 0
# Read the hook payload ONCE (stdin isn't seekable).
payload="$(cat)"
# Honor stop_hook_active: if the harness is already re-firing the Stop hook in a loop,
# let go — blocking again would wedge the session. (jq-free: grep the JSON.)
printf '%s' "$payload" | grep -q '"stop_hook_active"[[:space:]]*:[[:space:]]*true' && exit 0

# Session-scope the marker: /sweep writes ITS session id into .agent/active (the arm step).
# Release ONLY a session that is PROVABLY different from the one that armed it, so a
# concurrent host session (or a stale marker whose session is long gone) isn't held by
# someone else's sweep. Never weakens the sweep's own hold: an unknown id on either side
# (a legacy `touch` marker, an older CC with no session_id) falls through to the block.
armed=$(head -n1 "$CLAUDE_PROJECT_DIR/.agent/active" 2>/dev/null)
here=$(printf '%s' "$payload" | sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
[ -n "$armed" ] && [ -n "$here" ] && [ "$armed" != "$here" ] && exit 0

# Unclaimed work is a task folder in 00_todo/. (An in_progress task is the loop's to resume next
# iteration, so it doesn't block stopping.) Prefer coop's configured queue list on the host. Inside
# a box coop is deliberately absent, so discover the mounted repo's .agent/tasks dirs directly.
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
  [ -d "$q/00_todo" ] || continue
  n=$(find "$q/00_todo" -mindepth 2 -maxdepth 2 -name task.md 2>/dev/null | wc -l | tr -d ' ')
  left=$((left + ${n:-0}))
done < <(queue_paths)
if [ "${left:-0}" -gt 0 ]; then
  echo "$left unclaimed task(s) in 00_todo across the repo's queues. Keep going: claim or block the next task before stopping." >&2
  exit 2
fi
