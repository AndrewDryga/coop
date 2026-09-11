---
name: command-output-tiers
description: "Unprefixed human output, truthful progress, useful results, and readable consequences"
scope: cli-output
sources: [internal/ui/ui.go, internal/ui/usage.go, internal/ui/section.go, internal/box/launch_sections.go, internal/cli/launch_box.go, internal/cli/commands.go]
check: "none"
updated: 2026-09-11
---

# Command output: no prefixes, useful results, truthful progress

- Do not prefix human content with `coop:`, including after provider/build output. The command
  and concrete headings identify the speaker. Do not rewrite provider bytes or machine schemas.
- Active setup/launch work uses sentence-case sections with indented results. Reserve ✓ for
  a proved success, ⚠ for an actual caution, and ✗ for failure. `Starting Codex` has no success
  icon: the provider session has not yet completed.
- State what has actually happened. `The Coop box has stopped` follows confirmed stop, never
  the event that merely begins cleanup. While waiting, say `Stopping the Coop box…`.
  Separate process exit, stopped box, and remaining resource cleanup.
- Passive inspection shows useful facts and only-present issues, not ten healthy statuses.
  Doctor and active qualification still show the checks the user requested.
- Avoid routine current-directory headers and default image labels. Show paths when they
  identify an actionable file, different scope, ambiguity, or an exact destructive target.
  Show a nondefault image/fallback when it materially affects the operation or its evidence.
- Use blank lines between sections, two-space nesting, and six-space error causes. Standalone
  footer actions start at column zero. Indentation must mean actual nesting, not decoration.
- Rejected input is data, not a sentence a parser writes: build a `ui.UsageError` (the command
  path, the offending token, a usage line) and let it render — a leading blank line, one red `✗`
  headline naming the full command, an optional six-space cause stating the actual constraint,
  then aligned `Did you mean:` / `Usage:` / `Example:` / `Help:` rows; exit 2, stderr only.
  `internal/ui/usage.go` holds the constructors; `internal/cli/testdata/approved` pins the bytes.
- A deletion preview asks about a future action. Say "permanently delete" or "will be deleted",
  never "deleted" before confirmation. Put exact affected resources in a short list, followed
  by a simple confirmation. Do not repeat the same threat in three headings or list unrelated
  kept resources. Group retained data only when it resolves a real ambiguity.
- Runtime next actions are ordinary prose, not uppercase pseudo-option sections: "Please edit
  the preset template: <path>" and "To run it, use:". Keep short identity once where useful:
  "Signing in to claude@work", not a provider heading plus an account ledger.
- Network wording names the real concept: network access for mode; network rules for approved
  selectors/protocols/ports; network traffic for allowed/blocked activity; remote address for
  a recorded hostname/IP/port. Never use "website" as a synonym for an IP or ICMP rule.
- Active launch uses "Configuring network access" in every mode. A config file supplies the
  rules; it is not the component that enforces them.
- Bulk user-managed exceptions belong in an editable file with stable entries, not a hundred
  per-item commands. Precise scope and persistence are part of the design, not cosmetic labels.
- Actionable details beat state ledgers: account detail gives launch/default/sign-in actions;
  preset lists explain the lead/roles relationship; AI-facing helper results report actual work
  rather than repeating the lead's standing instructions.
- Green success, amber warning, red error, cyan emphasis, dim optional hints. Do not dim error
  causes or remedies. Pad columns before styling; preserve meaning without color and in pipes.
- Derive next actions from actual state and label them. Keep setup's per-file logs subordinate
  to meaningful results; never repeat a redundant final "all okay" ledger.

**How to apply:** reuse the existing UI helpers and renderers; change their prefix behavior
as needed rather than introducing another styling layer. Keep raw command output, ACP, JSON,
completion scripts, and supervised bounded logs on their existing streams. An in-app task design
is not proof the live helpers conform; regression tests must exercise all output tiers.

See also [[help-output-style]] and [[no-color-in-width-fields]].

## Changelog
- 2026-09-11 — the shared input-error renderer (a2cfb50, `ui.UsageError`) joins the card as the one
  shape rejected input takes; its fixtures are gated byte for byte.
- 2026-09-11 — second full correction batch: swept login, preset, loop, deletion, network and
  session fixtures. Made pre-delete tense explicit, simplified runtime next actions, normalized
  network rule/traffic/address terms, and moved hash-heavy settings behind useful permissions.
  Current source still has website copy in launch_sections/net_approve/help/init; removal and
  fixture tests are in the same design task. No implementation compliance is claimed.
- 2026-09-11 — superseded the old single-`coop:` anchor and after-provider exception. Swept
  launch/teardown, task/watch, account/preset, network, services/build/doctor, session and helper
  examples in the CLI design task. Removed routine scope/image noise; tied completed-stop copy
  to stop evidence; grouped deletion consequences. Existing UI source still needs the task's
  implementation and rendering tests; `check: none` does not claim a new gate.

### Earlier history

These entries record earlier decisions; the current rule above supersedes conflicting guidance.

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
