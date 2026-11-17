#!/bin/bash
# Claude activates this hook only while /sweep is active. Queue state is authoritative on every
# Stop attempt: the model can release itself by finishing or blocking each actionable task.
#
# Actionable means: every folder in 00_todo/, and every folder in 10_in_progress/ EXCEPT one someone
# else holds: a fork's assignment, a live lease (a running `coop loop`), or another agent's live
# claim. `coop tasks queues --held` lists those folders. A sweep may neither finish such a task (it
# would overwrite that work) nor block it (it would park someone else's in-flight task). A dead
# claimant, an unbound claim, or a claim held by this very agent (coop finds its pid in its own
# ancestry) stays actionable. Without `coop` on PATH, or when it cannot list held tasks, the hook
# counts everything.

queue_paths() {
  if command -v coop >/dev/null 2>&1; then
    paths=$(cd "$CLAUDE_PROJECT_DIR" && coop tasks queues) || return 1
    printf '%s\n' "$paths"
    return
  fi
  find "$CLAUDE_PROJECT_DIR" -type d -path '*/.agent/tasks' -prune -print
}

# held_tasks prints the in-progress task folders someone else holds, one per line. Empty when coop
# is unavailable or cannot tell, so every task then counts.
held_tasks() {
  command -v coop >/dev/null 2>&1 || return 0
  (cd "$CLAUDE_PROJECT_DIR" && coop tasks queues --held </dev/null 2>/dev/null) || return 0
}

paths=$(queue_paths) || {
  echo "Sweep queue guard could not discover task queues; refusing to stop. Fix the reported error and retry." >&2
  exit 2
}
held=$(held_tasks)
left=0
foreign=0
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
    while IFS= read -r task; do
      [ -n "$task" ] || continue
      if [ "$state" = 10_in_progress ]; then
        case $'\n'"$held"$'\n' in
          *$'\n'"$task"$'\n'*)
            foreign=$((foreign + 1)) # a fork, a running loop or another live agent holds it
            continue ;;
        esac
      fi
      left=$((left + 1))
    done <<< "$tasks"
  done
done <<< "$paths"
if [ "${left:-0}" -gt 0 ]; then
  note=""
  [ "$foreign" -gt 0 ] && note=" ($foreign in progress that a fork, a running loop or another live agent holds were left alone.)"
  echo "$left actionable task(s) remain in 00_todo or 10_in_progress across the repo's queues. Keep sweeping: finish or block every task before stopping.$note" >&2
  exit 2
fi
