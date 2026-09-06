#!/bin/bash
# Claude activates this hook only while /sweep is active. Queue state is authoritative on every
# Stop attempt: the model can release itself by finishing or blocking each actionable task.
#
# Actionable means: every folder in 00_todo/, and every folder in 10_in_progress/ EXCEPT one whose
# claim names another agent's live process. A claim is bound to the claiming agent's pid
# (`coop tasks claim`; `coop tasks ls` prints "claimed by codex (pid N)"), and the loop never adopts a
# task whose claimant is alive — so neither may a sweep finish it (it would overwrite that agent's
# uncommitted work) nor block it (it would park someone else's in-flight task). A dead claimant, an
# unbound claim, or a claim held by this very agent (the pid sits in this hook's own ancestry) stays
# actionable. Without `coop` on PATH the hook counts everything, as it always did.

queue_paths() {
  if command -v coop >/dev/null 2>&1; then
    paths=$(cd "$CLAUDE_PROJECT_DIR" && coop tasks queues) || return 1
    printf '%s\n' "$paths"
    return
  fi
  find "$CLAUDE_PROJECT_DIR" -type d -path '*/.agent/tasks' -prune -print
}

# The pids above this hook: the sweeping agent and its parents. A claim held by one of them is ours.
ancestry() {
  p=$$
  while [ "${p:-0}" -gt 1 ]; do
    printf ' %s' "$p"
    p=$(ps -o ppid= -p "$p" 2>/dev/null | tr -d ' ')
    [ -n "$p" ] || break
  done
  printf ' '
}

# claimant_pids prints "<task id> <pid>" per in-progress task whose claim is process-bound, from
# the one listing coop prints for every configured queue. Empty when coop is unavailable.
claimant_pids() {
  command -v coop >/dev/null 2>&1 || return 0
  listing=$(cd "$CLAUDE_PROJECT_DIR" && coop tasks ls --in-progress </dev/null 2>/dev/null) || return 0
  printf '%s\n' "$listing" | sed -n 's/.*claimed by [^(]*(pid \([0-9][0-9]*\)).*  \([^ ][^ ]*\)$/\2 \1/p'
}

paths=$(queue_paths) || {
  echo "Sweep queue guard could not discover task queues; refusing to stop. Fix the reported error and retry." >&2
  exit 2
}
own=$(ancestry)
claims=$(claimant_pids)
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
        pid=$(printf '%s\n' "$claims" | awk -v id="$(basename "$task")" '$1 == id { print $2; exit }')
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && [ "${own#* "$pid" }" = "$own" ]; then
          foreign=$((foreign + 1)) # another agent's live claim: not ours to finish or block
          continue
        fi
      fi
      left=$((left + 1))
    done <<< "$tasks"
  done
done <<< "$paths"
if [ "${left:-0}" -gt 0 ]; then
  note=""
  [ "$foreign" -gt 0 ] && note=" ($foreign in progress under another live agent's claim were left alone.)"
  echo "$left actionable task(s) remain in 00_todo or 10_in_progress across the repo's queues. Keep sweeping: finish or block every task before stopping.$note" >&2
  exit 2
fi
