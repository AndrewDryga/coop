---
name: command-output-tiers
description: "no tool prefix on human output: a dim progress log, a plain anchor line, a bright next-steps block, outcome glyphs"
scope: cli-output
sources: [internal/ui/ui.go, internal/ui/usage.go, internal/ui/section.go, internal/box/launch_sections.go, internal/cli/launch_box.go, internal/cli/commands.go]
check: "none"
updated: 2026-09-11
---

# Command output: dim log, one plain anchor, a bright "next steps" block

A command that scaffolds or sets things up (e.g. `coop init`) emits three visually distinct
tiers, so the log of what happened never drowns out what the user must do next:

- **Routine progress** — every "wrote X", "linked X", "added skill X", "commit gate: Y" — is a
  faint, indented `ui.Detail` line. A long run reads as one quiet block.
- **One closing anchor** — a single `ui.Note("scaffolded into <repo>")` closes the dim block at
  normal brightness. NO `coop:` prefix: human output never carries a tool prefix, so the line
  reads as the sentence it is.
- **Next steps** — the actions the user runs next go in a `ui.Steps(...)` block: a blank line, a
  bold `next steps:` header, one cyan-`→` line per action. Assemble them in the CALLER from what
  actually landed (a build step only if a `Dockerfile.agent` exists, `coop up` only if services
  were added), not a fixed script — and never inline among the progress log.
- **A standalone result** — a synchronous query/result command (`coop tasks` …, `coop check-secrets`,
  `coop credentials`, `coop fork review`, `coop tasks watch`) prints only its own outcome,
  with no agent output and no dim progress block to stand out from. Voice it by outcome, with NO
  command-name echo: `ui.OK` (green ✓) a success, `ui.Warn` (amber ⚠) a non-fatal caution,
  `ui.Error` (red ✗) a failure, `ui.Note` (plain) a neutral note. State the *result* — `tasks lint:
  clean` becomes `✓ no issues — 12 tasks checked`.
- **An interactive launch's sections** — the host-side work a person watches BEFORE the agent's
  output begins (`coop claude`, `coop run`) — are bold, unprefixed headings (`ui.Section`) with
  indented results (`ui.Pass` ✓ / `ui.Caution` ⚠): the invocation already says who is speaking.
  `Checking the Coop box` (only when there is a repair to do),
  `Protecting secrets`, one `Internet access` heading whatever the mode, `Starting <Agent>` with no
  success glyph because the agent's output IS the result. A section that fails ends with `ui.Fail`:
  the indented red ✗ headline, a blank line, the concrete reason six spaces further in, a blank
  line, the remedy one level under the section — reason and remedy never dimmed — and the error
  comes back `ui.Reported`, so the dispatcher's fallback `✗` does not repeat it. What coop says
  AFTER agent output — `stopping the box — …`, `Network run <id>` — is plain too; following a
  provider's output is not a reason to stamp a prefix on it.
- **Rejected input** — a usage error is not a sentence a parser writes. Build a `ui.UsageError`
  from DATA (the command path, the offending token, a usage line) and let it render: a leading
  blank line, one red `✗` headline, an optional six-space cause, then two-space `Did you mean:` /
  `Usage:` / `Example:` / `Help:` rows whose commands align past the widest label. Exit 2, stderr
  only, stdout empty. `internal/ui/usage.go` holds the constructors; `internal/cli/testdata/approved`
  pins the bytes.

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
- Routine per-file/per-step progress → `ui.Detail` (dim, indented).
- A status line coop speaks in its own voice → `ui.Note` (plain, normal brightness). There is no
  prefixing helper: `ui.Info` was retired, and a literal `"coop: "` in human output is a bug.
- Standalone command results (`coop tasks`, `coop check-secrets`, `coop credentials`,
  `coop fork review`, `coop tasks watch`, …) → a glyph helper by outcome: `ui.OK` ✓ / `ui.Warn` ⚠ /
  `ui.Error` ✗ / `ui.Note` (neutral), and never echo the command name you were invoked as (no
  `tasks lint:` / `check-secrets:` prefix).
- Errors (every returned error reaches the user through `ui.Error`'s red ✗) say what failed AND how
  to fix it — name the file/flag and the exact command to run, not just the symptom. Rejected INPUT
  goes through `ui.UsageError` instead, so every command refuses the same way.
- Machine surfaces are untouched by all of this: `--json`, the ACP/JSON-RPC streams, generated file
  comments and git reflog messages keep their own identifiers.
- Next-step actions → collect a `[]string` in the command and pass it to `ui.Steps`; derive each
  step from real state (see `initNextSteps` in `internal/cli/commands.go`).
- Pre-launch work on an interactive run → `ui.Section` / `ui.Pass` / `ui.Caution` / `ui.Fail`
  (`internal/box/launch_sections.go`, `internal/cli/launch_box.go`), gated on the run being
  interactive (`!Batch && !Quiet && !ForceNoTTY`) so a loop, a probe and an ACP child keep their
  bounded one-line log. A remedy names the command to REPEAT (`run 'coop codex' again`), never a
  manual detour the launch retries itself (`coop build`).

See also [[help-output-style]] and [[no-color-in-width-fields]].

## Changelog
- 2026-09-11 — the approved CLI review removed the `coop:` prefix from ALL human output, including
  after provider output: `ui.Info` is retired (its 87 call sites now call `ui.Note`), the network
  summary's `View.Prefix` field is gone, and the literal `coop: ` was stripped from the box
  entrypoint's supervisor lines, the serve-port notices and the self-update notices. Left alone
  deliberately: the ACP/JSON-RPC payloads in `internal/acpproxy` and `internal/acpctl`, the
  generated `mcp.json` comment, the `coop: re-sign commits` git reflog message and the
  `.git/info/exclude` marker — protocols, file markers and third-party surfaces, not human output.
  Added the rejected-input tier (`ui.UsageError`) from the same review.
- 2026-09-10 — added the interactive-launch sections tier (`ui.Section`/`Pass`/`Caution`/`Fail`,
  `ui.Reported`) from the network-output redesign: the user asked for named sections instead of
  isolated `coop:` status lines and a `coop build` nag on every launch. Swept `internal/box/run.go`
  and `internal/cli/commands.go`: the `shadowed N secret path(s)` and skew-nudge `ui.Info` lines
  became sections on the interactive path; the loop/ACP/quiet paths keep theirs by design.
- 2026-08-25 — replaced Fleet watch examples with the surviving task watch standalone result;
  output-tier guidance is unchanged.
- 2026-06-19 — created
- 2026-07-04 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: swept `cmdInit` (the rule's own canonical example,
  commands.go) and catalogued every `ui.Info` call site across internal/cli (~80 hits) by
  containing command. 2 violations found: (1) internal/cli/sign.go:213 — `coop sign` (a
  standalone, non-interleaved result command) uses `ui.Info("nothing to sign...")`; its own
  sibling success path at sign.go:216 correctly uses `ui.OK(...)` — the null-result line should be
  `ui.Note` per the "standalone result" tier, not the `coop:`-anchored voice. (2)
  commands.go's `cmdInit` (~1506-1534) fires 2 (3 for a monorepo) sequential `coop:`-prefixed
  `ui.Info` lines back to back ("monorepo: ...", "scaffolded into ...", "per-agent dirs:
  .../no agents signed in...") instead of the rule's prescribed single closing anchor. Both
  queued for the lead. Not flagged: the once-daily update-notice (updatecheck.go:150, deferred
  after every command in cli.go:75) also fires `ui.Info` after standalone commands — judged
  defensible, since it's a cross-cutting aside orthogonal to the invoked command's own tiering,
  not a violation of this rule.
- 2026-08-09 — fixed sweep: sign.go's nothing-to-sign line is now `ui.Note`, matching its sibling
  `ui.OK` on the success path. `cmdInit` now closes on exactly one `ui.Info` anchor
  ("already initialized at .../scaffolded into ..."): the monorepo member-count line and the
  per-agent-dirs/no-agents-signed-in line are demoted to `ui.Detail`, moved ahead of the anchor so
  the dim log still reads top-to-bottom before the closing line; the `ui.Warn` for an unregistered
  subproject is untouched (a real warning, not part of this violation). No output-assertion test
  covered either exact string (grepped *_test.go and testdata; `TestInitNextSteps` only exercises
  the `initNextSteps` helper, not `cmdInit`'s printed lines) — none to update. `cmdInit` is once
  again the rule's own compliant example.
