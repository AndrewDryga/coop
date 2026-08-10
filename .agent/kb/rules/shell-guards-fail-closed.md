---
name: shell-guards-fail-closed
description: "a shell guard checks every command's status and fails CLOSED on ambiguity; a script that fails open says so in its header"
scope: agent-workflow
sources: [.agent/skills/sweep/queue-guard.sh, internal/scaffold/templates/skills/sweep/queue-guard.sh, internal/scaffold/queue_guard_test.go, .claude/hooks/commit-gate.sh]
check: "go test ./internal/scaffold -run TestSweepQueueGuard"
updated: 2026-08-10
---

# A shell guard fails closed, and says which way it fails

A GUARD is a script whose exit status decides whether it is safe to stop or proceed — the sweep's
Stop hook is the one this repo ships. In a guard, every command's status is captured and checked,
and anything ambiguous exits non-zero with a diagnostic naming what to fix. Three shell idioms turn
a failure into a confident "nothing found", so a guard may not use them:

- `cmd 2>/dev/null` in a command substitution — the error vanishes and the empty result reads as "no
  work left". Redirect only where the status is separately checked.
- `find … | wc -l` — the pipeline's status is `wc`'s, which always succeeds; a `find` that failed
  counts as `0`.
- `<(…)` process substitution — the inner command's status never reaches the enclosing shell.

A script that deliberately fails OPEN is fine — it must say so in its header, the way
`.claude/hooks/commit-gate.sh:3` does ("Fails open."), so nobody mistakes it for a guard.

**Why:** the sweep guard shipped fail-open and released the agent while work remained. `7fd8150`
(2026-07-14) shows the exact before/after: `paths=$(coop tasks queues 2>/dev/null)` swallowed a
failing `coop` and then `[ -n "$paths" ]` treated the empty string as "no queues", and
`n=$(find … 2>/dev/null | wc -l)` counted a failed `find` as zero — a discovery or count failure
silently ended the sweep with tasks still in `00_todo/`. A guard that cannot tell "nothing to do"
from "I could not look" is worse than no guard: it reports success either way.

**How to apply:**
- Capture, then branch: `out=$(cmd) || { echo "…; refusing to stop." >&2; exit 2; }`. Count by
  reading lines, not by piping to `wc`.
- Say what to fix in the diagnostic. "Sweep queue guard cannot count `<path>`; refusing to stop" is
  actionable; a bare non-zero exit is not.
- Reject the shapes you cannot verify (a symlinked queue root, an unreadable configured path) rather
  than skipping them quietly.
- Both copies of the guard — this repo's `.agent/skills/sweep/` and the scaffold template shipped by
  `coop init` — must stay identical; `TestSweepQueueGuard` runs the template one, including its
  "configured discovery failure fails closed" and "count failure fails closed" subtests.

Related: [[fix-the-bug-not-the-feature]] (a guard that keeps firing is fixed at the cause, never by
loosening it) and [[static-bounded-supervision]].

## Changelog
- 2026-08-10 — created, promoting the `kb/inbox/shell-guards-fail-closed.md` draft that `7fd8150`
  left behind (its sibling became [[hermetic-git-tests]]). **Swept every shell surface in the repo:**
  4 `.sh` files plus the box entrypoint heredoc (`internal/box/image.go:115`). **0 violations.** Two
  are the guard itself (`.agent/skills/sweep/queue-guard.sh` and its scaffold template — byte
  identical, both fail closed on discovery and count failures since `7fd8150`); `install.sh` runs
  `set -eu` with no `wc` pipeline or process substitution; `.claude/hooks/commit-gate.sh` is the
  documented fail-open exception; the box entrypoint is best-effort provisioning (`|| true` on
  purpose), not a guard, so the rule does not reach it. The rule is therefore PREVENTIVE — it holds
  the fix in place rather than describing a live mess. Two gaps found and reported for the queue:
  `make shellcheck` lints `install.sh` only (neither guard copy nor the commit hook), and nothing
  pins the two guard copies byte-identical.
