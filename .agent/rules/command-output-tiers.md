# Command output: dim log, one `coop:` anchor, a bright "next steps" block

A command that scaffolds or sets things up (e.g. `coop init`) emits three visually distinct
tiers, so the log of what happened never drowns out what the user must do next:

- **Routine progress** — every "wrote X", "linked X", "added skill X", "commit gate: Y" — is a
  faint, indented `ui.Detail` line with NO `coop:` prefix. A long run reads as one quiet block.
- **One `coop:` anchor** — a single `ui.Info("scaffolded into <repo>")` marks coop's voice and
  closes the dim block. Don't prefix every line with `coop:`.
- **Next steps** — the actions the user runs next go in a `ui.Steps(...)` block: a blank line, a
  bold `next steps:` header, one cyan-`→` line per action. Assemble them in the CALLER from what
  actually landed (a build step only if a `Dockerfile.agent` exists, `coop up` only if services
  were added), not a fixed script — and never inline among the progress log.
- **A standalone result** — a synchronous query/result command (`coop tasks` …, `coop check-secrets`,
  `coop profiles`, `coop status`, `coop fork review`, `coop fleet watch`) prints only its own outcome,
  with no agent output and no dim progress block to stand out from, so it needs no `coop:` anchor. Voice it by outcome, with NO prefix and NO
  command-name echo: `ui.OK` (green ✓) a success, `ui.Warn` (yellow ⚠) a non-fatal caution,
  `ui.Error` (red ✗) a failure, `ui.Note` (plain) a neutral note. State the *result* — `tasks lint:
  clean` becomes `✓ no issues — 12 tasks checked`. (A command that interleaves with an agent —
  fork/loop/run — or runs multi-step still uses the `coop:`-prefixed `ui.Info`.)

**Why:** the user pasted 28 identical `coop:` lines and said "it makes it very hard to see what
user needs to do and what is actual log… add some spacing, coloring and formatting." A flat,
uniformly-prefixed stream buries the 3 lines that matter behind 25 that don't. Later, on
`coop tasks decisions` printing `coop: no open decisions — nothing is blocked`, the user said the
prefix is "not needed when it's clear what outputs it" — a command you invoked directly, with
nothing else writing to the terminal, is exactly that clear case. Then, on `coop tasks lint`
printing `coop: tasks lint: clean`, the user said it's "not human readable — prefix, then task name
again" and asked for nice outputs and super-clear errors — hence the ✓/⚠/✗ result glyphs, dropping
the command-name echo, and errors that name the fix.

**How to apply:**
- Routine per-file/per-step progress → `ui.Detail` (dim, indented, no prefix), never `ui.Info`.
- Standalone command results (`coop tasks`, `coop check-secrets`, `coop profiles`, `coop status`,
  `coop fork review`, `coop fleet watch`, …) → a glyph helper by outcome: `ui.OK` ✓ / `ui.Warn` ⚠ /
  `ui.Error` ✗ / `ui.Note` (neutral). Never `ui.Info`, and never echo the command name you were
  invoked as (no `tasks lint:` / `check-secrets:` prefix).
- Errors (every returned error reaches the user through `ui.Error`'s red ✗) say what failed AND how
  to fix it — name the file/flag and the exact command to run, not just the symptom.
- Reserve `ui.Info` (the bold-cyan `coop:` prefix) for the summary anchor that CLOSES a dim progress
  block (e.g. `coop init`), and for an operation whose progress interleaves with agent or tool output:
  `coop run`/`fork` (the agent session), `coop loop` (each iteration), `coop build`/`update`/`up`
  (docker output), `coop fork merge` (gate + rebase), plus a backgrounding op's confirmation that pairs
  with that voice (`coop fork … --loop -d`). A read-only command that only prints its own result —
  `coop status`, `coop fork review`, `coop fleet watch` — is NOT one of these; use the plain/glyph voice.
- Next-step actions → collect a `[]string` in the command and pass it to `ui.Steps`; derive each
  step from real state (see `initNextSteps` in `internal/cli/commands.go`).

See also [[help-output-style]] and [[no-color-in-width-fields]].
