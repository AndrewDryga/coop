#!/bin/bash
# Refuse to stop while the queue has unclaimed work. Armed only during a batch
# (when .agent/active exists), so it never nags during interactive use.
[ -f "$CLAUDE_PROJECT_DIR/.agent/active" ] || exit 0
# Honor stop_hook_active: if the harness is already re-firing the Stop hook in a loop,
# let go — blocking again would wedge the session. (jq-free: grep the JSON on stdin.)
printf '%s' "$(cat)" | grep -q '"stop_hook_active"[[:space:]]*:[[:space:]]*true' && exit 0
q="$CLAUDE_PROJECT_DIR/.agent/TASKS.md"; [ -f "$q" ] || exit 0
# Count only real task lines ("- [ ]" at line start), so the "[ ]" in the legend
# header or an Example section isn't mistaken for unclaimed work.
left=$(grep -cE '^- \[ \]' "$q" 2>/dev/null || echo 0)
if [ "$left" -gt 0 ]; then
  echo "TASKS.md has $left unclaimed item(s). Keep going, or mark a blocker [B]." >&2
  exit 2
fi
