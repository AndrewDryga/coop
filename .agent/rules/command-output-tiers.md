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
  `coop profiles` …) prints only its own outcome, with no agent output and no dim progress block to
  stand out from, so it needs no `coop:` anchor either: use `ui.Note` — a plain stderr line with NO
  prefix. (A command that interleaves with an agent — fork/loop/run — or runs multi-step still uses
  `ui.Info`.)

**Why:** the user pasted 28 identical `coop:` lines and said "it makes it very hard to see what
user needs to do and what is actual log… add some spacing, coloring and formatting." A flat,
uniformly-prefixed stream buries the 3 lines that matter behind 25 that don't. Later, on
`coop tasks decisions` printing `coop: no open decisions — nothing is blocked`, the user said the
prefix is "not needed when it's clear what outputs it" — a command you invoked directly, with
nothing else writing to the terminal, is exactly that clear case.

**How to apply:**
- Routine per-file/per-step progress → `ui.Detail` (dim, indented, no prefix), never `ui.Info`.
- Standalone command results (`coop tasks`, `coop check-secrets`, `coop profiles`, …) → `ui.Note`
  (plain, no prefix), never `ui.Info`.
- Reserve `ui.Info` (the bold-cyan `coop:` prefix) for the summary anchor that CLOSES a dim progress
  block (e.g. `coop init`) and for the live loop, where it keeps coop's voice distinct from agent output.
- Next-step actions → collect a `[]string` in the command and pass it to `ui.Steps`; derive each
  step from real state (see `initNextSteps` in `internal/cli/commands.go`).

See also [[help-output-style]] and [[no-color-in-width-fields]].
