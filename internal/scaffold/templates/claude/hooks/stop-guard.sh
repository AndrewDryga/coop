#!/bin/bash
# Refuse to stop while the queue has unclaimed work. Armed only during a batch
# (when .agent/active exists), so it never nags during interactive use.
[ -f "$CLAUDE_PROJECT_DIR/.agent/active" ] || exit 0
# Honor stop_hook_active: if the harness is already re-firing the Stop hook in a loop,
# let go — blocking again would wedge the session. (jq-free: grep the JSON on stdin.)
printf '%s' "$(cat)" | grep -q '"stop_hook_active"[[:space:]]*:[[:space:]]*true' && exit 0

# Unclaimed work is a task folder in 00_todo/. (An in_progress task is the loop's to resume
# next iteration, so it doesn't block stopping.)
tasks="$CLAUDE_PROJECT_DIR/.agent/tasks"
[ -d "$tasks" ] || exit 0
left=$(find "$tasks/00_todo" -mindepth 2 -maxdepth 2 -name task.md 2>/dev/null | wc -l | tr -d ' ')
if [ "${left:-0}" -gt 0 ]; then
  echo ".agent/tasks/00_todo has $left unclaimed task(s). Keep going ('coop tasks claim <id>'), or 'coop tasks block <id>'." >&2
  exit 2
fi
