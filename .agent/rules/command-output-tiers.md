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

**Why:** the user pasted 28 identical `coop:` lines and said "it makes it very hard to see what
user needs to do and what is actual log… add some spacing, coloring and formatting." A flat,
uniformly-prefixed stream buries the 3 lines that matter behind 25 that don't.

**How to apply:**
- Routine per-file/per-step progress → `ui.Detail` (dim, indented, no prefix), never `ui.Info`.
- Reserve `ui.Info` (the bold-cyan `coop:` prefix) for a summary anchor and for the live loop,
  where it keeps coop's voice distinct from agent output.
- Next-step actions → collect a `[]string` in the command and pass it to `ui.Steps`; derive each
  step from real state (see `initNextSteps` in `internal/cli/commands.go`).

See also [[help-output-style]] and [[no-color-in-width-fields]].
