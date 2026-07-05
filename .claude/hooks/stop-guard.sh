#!/bin/bash
# Refuse to stop while the queue has unclaimed work. Armed only during a batch
# (when .agent/active exists), so it never nags during interactive use.
[ -f "$CLAUDE_PROJECT_DIR/.agent/active" ] || exit 0
# Honor stop_hook_active: if the harness is already re-firing the Stop hook in a loop,
# let go — blocking again would wedge the session. (jq-free: grep the JSON on stdin.)
printf '%s' "$(cat)" | grep -q '"stop_hook_active"[[:space:]]*:[[:space:]]*true' && exit 0

# Unclaimed work is a task folder in 00_todo/. (An in_progress task is the loop's to resume next
# iteration, so it doesn't block stopping.) `coop tasks queues` prints every configured queue's path
# — one .agent/tasks, or each subproject's in a monorepo (.agent/project.yaml) — so a monorepo /sweep
# sees them all. coop is on PATH during a sweep; if it can't answer, nothing prints and we let go.
left=0
while IFS= read -r q; do
  [ -d "$q/00_todo" ] || continue
  n=$(find "$q/00_todo" -mindepth 2 -maxdepth 2 -name task.md 2>/dev/null | wc -l | tr -d ' ')
  left=$((left + ${n:-0}))
done < <(cd "$CLAUDE_PROJECT_DIR" && coop tasks queues 2>/dev/null)
if [ "${left:-0}" -gt 0 ]; then
  echo "$left unclaimed task(s) in 00_todo across the repo's queues. Keep going ('coop tasks claim <id>'), or 'coop tasks block <id>'." >&2
  exit 2
fi
