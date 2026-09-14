---
name: loop-output-is-task-centered
description: loop output groups startup and environment work around dim task banners and canonical per-attempt targets
scope: cli-output
sources: [internal/loop/report.go, internal/loop/loop.go, internal/loop/iteration.go, internal/loop/streamjson.go, internal/loop/streamjson_providers.go, internal/loop/banners.go, internal/box/launch_sections.go, internal/box/run.go, internal/box/filtered_services.go, internal/box/services_note.go, internal/cli/acp_cmd.go, internal/cli/commands.go, internal/cli/boxsweep.go, internal/cli/loop_cmd.go, internal/tasks/dir.go, internal/tasks/cmd.go, internal/cli/help.go, internal/ui/ui.go, internal/ui/wrap.go]
check: make check
updated: 2026-09-14
---

# Center loop output on the task and the actual attempt

- Start with `Using .agent/loop.yaml` (or truthful built-in defaults), a blank line, and the
  applicable Ctrl-C paragraph. Do not repeat `Coop loop` or lead with an opaque config digest.
- Group startup recovery, stale-image cautions and proved caffeinate startup under `Preparing
  loop`. Keep `Running pre-flight checks`, answered-blocker results and released gone-owner
  claims. A recovered filtered run uses an amber warning, per the approved operator layout.
- A task banner has horizontal rules, `Task N - Attempt M`, a blank line, the title, another
  blank line, and canonical Agent plus Queue rows. Only the title is normal foreground;
  all other banner text and borders are dim. Keep ordinals stable and count each attempt once.
- Queue rows name actual completed/active/pending/blocked folder counts, using the assignment's
  post-claim snapshot. They are not completed-this-run totals. Rendering must never claim a task
  or manufacture an in-progress count. Keep useful scope when multiple queues are selected.
- Nest secret protection, network access and service startup under
  `Preparing task environment`. Reuse existing proof and narration helpers. Preserve every
  actionable warning, including masked service secrets; show only facts the launch established.
  A credential or rate-limit fallback from a clean, unchanged worktree reuses the prepared service
  stack and does not print the same environment block again. A changed or dirty attempt prepares
  services normally rather than trusting stale setup.
- Loop work/preflight/review sessions do not automatically publish project `serve.ports`, so
  they have no Publishing ports section. Remove generated host bindings and false host-access
  notes/environment, not just the displayed URLs. Preserve ACP/interactive publication and
  independent sibling-service connectivity, in-box tests and browser automation.
- Use `Starting provider[:model][/effort][@credential]`, e.g.
  `Starting claude:claude-opus-5@personal`. Each task, review and fallback has its own selected
  target. Preserve the provider-reported model where available and never invent a resolved ID.
  A failure before the provider's identity boundary needs no fabricated Starting heading.
- Replace only Coop's generated `using … model … credential …` init narration. Do not strip
  provider text, raw events, diagnostics, usage or watchdog activity. Keep identity changes visible.
- Preserve review/recovery/wait/denial/interrupt/final-result distinctions and their remedies.
  Task completion is not final-review success; no-actionable, busy and blocked are not all-passed.
  When an explicitly enabled final verification cannot run or return an accepted verdict, keep
  completed tasks intact but end nonzero with an unverified result; never follow its failure with
  an all-passed banner or claim a fresh loop will recover historical verification subjects.
- A failed tool row keeps its identity and exit status while leaving room for an available
  cause. Long commands must not consume the whole live row. Skip generic leading exit-code
  boilerplate when a useful failure follows; preserve meaningful MCP errors and never replace
  a generic error with unrelated trailing metadata. Raw provider evidence stays unchanged.
- Ordinary shell activity may use a useful provider-supplied purpose with a reserved command
  preview, never invented narration. Missing/generic descriptions and very narrow terminals
  keep the command-only fallback. Role/task classifications and matched failure command identity
  take precedence; display shortening does not alter the full provider event.
- Give reviews compact thin-rule stage/result banners, with their own reviewer and affected
  work. A valid review or verification reopen continues automatically within the shared round
  budget; only a real stop, cap, ownership boundary or error justifies a stopping remedy.
- Keep quota fallback, missing-commit repair and busy-queue explanations to one sentence each.
  Human answer recommendations use `coop tasks decisions -i`, including sibling task/help
  surfaces; listing-only documentation and noninteractive reads need not become interactive.
- Completed-task rows show real checked/total subtasks when defined. Use the parsed checklist,
  never infer full completion from the folder state or invent a 0/0 count for unavailable data.
- Continue commands retain the actual target/preset and explicitly selected queues, safely
  quoted; do not imply an exact last-task/review resume or invent a task-ID argument. Durable
  pending review needs its own trusted state and cannot be reconstructed from a starting task.
- An offline mode that prevents the selected provider from working is a RED failure, including
  `⚠ Offline — Claude cannot reach Anthropic`; do not choose severity from the icon alone.
- Use the shared colors and live sink: headings bold, success green, warnings amber, failures
  red, URLs cyan; warning causes/remedies normal. Wrap/pad before styling and remain readable
  without color. Preserve the existing live bar and static nonterminal/TERM=dumb behavior.

**Why:** on 2026-09-12 the user corrected run-wide agent identity because tasks can fall back,
asked for task statistics in a banner, retained recovery/preflight/image messages, rejected the
redundant `Coop loop ·` prefix, and specified: “only ‘Publish the Emisar Cursor Marketplace plugin’
in the task banner should be normal color.” The final identity correction was
`Starting claude:claude-opus-5@personal`. The user approved task authoring, not runtime changes
in that turn.

**How to apply:** reuse the loop voice, UI and box-launch boundaries rather than change Batch
execution semantics or build a second TUI. See [[command-output-tiers]],
[[no-color-in-width-fields]], [[static-bounded-supervision]] and the descriptive loop-live-bar
card. The full transcript and failure matrix are in queued task
`2026-09-12-polish-loop-output-with-task-banners-and-grouped`.

## Changelog
- 2026-09-14 — a real Emisar Frontier recovery printed and ran the same service preparation twice
  when the first credential was already rate limited. Swept work and review retries: both now reuse
  the stack only across a clean identical HEAD, while changed/dirty work remains a fresh setup.
- 2026-09-13 — made failed enabled final verification part of the terminal verdict and exit
  contract while preserving completed task state, original diagnostics and queue-specific exits.
- 2026-09-13 — swept ordinary Bash, role/task classifications and matched failure history;
  preserved real command identity while rendering supplied purposes with width-aware budgets.
  Focused event tests cover control/multiline/generic fallback, resize and unchanged success/
  failure behavior. Real installed terminal qualification and full gate deferred by explicit
  user batching; these deterministic examples alone are not the installed-command proof.
- 2026-09-13 — reproduced Claude's missing-log-directory result displaying only Exit code 1,
  and long labels hiding the diagnostic at 38/80 columns. Swept Claude, Codex and Gemini failure
  paths: Claude now skips generic boilerplate, text blocks retain boundaries, and the shared
  live row reserves cause space while keeping Codex exit suffixes. Static caps, raw traces,
  tool completion and provider-limit classification remain unchanged. Added native-shaped
  event controls and real CLI process checks for static and normal narrow terminals.
- 2026-09-12 — swept startup/sweep, task banner/counts, box setup, provider initialization,
  reviews/retries, network/final reports and live-bar source families. Current deviations:
  late Working-through intro; ungrouped startup/preflight; banner without attempt/counts;
  Batch disables shared environment sections; provider init still prints using/model/credential.
  Queued all in `2026-09-12-polish-loop-output-with-task-banners-and-grouped`; no runtime
  implementation is claimed. `check: none` until meaningful output/CLI fixtures exist and run.

- 2026-09-12 — applied the user's preview corrections: dash/Attempt caption, compact messages,
  red unusable offline mode, review/result banners, subtask counts and interactive answer
  remedies. Swept loop review/verify, continuation grammar, parsed task counts and task/help
  guidance. Found verification reopens stopping, lost explicit queue scope, summary counts
  discarded, and noninteractive answer hints in loop/task/help plus generated docs/fixtures.
  Queued those focused changes in the same output task. Pending-review restart durability
  already belongs to `2026-09-12-preserve-pending-final-review-across-loop-pauses`; no runtime
  changes or new task-selection flag are claimed. Future real CLI tests must graduate this rule.

- 2026-09-12 — user approved the colored previews and corrected loop project-port publication
  as an ACP/interactive concern. Swept runIteration, open assembleArgs/appendPublish, filteredPublish,
  servePublicationPlan/servicesNote and ACP/interactive Serve opt-ins. Loops currently set
  Serve true; servePublicationPlan also ignores that flag and can create false host-access notes.
  Queued the targeted behavior/note fix and both-path tests in the same output task; keep
  sibling-service ports/forwarders separate. Previews updated; runtime remains unimplemented.

- 2026-09-12 — implemented the approved lifecycle hierarchy across loop, box-launch and provider
  stream boundaries. Added exact plain/color/wrap fixtures plus real-binary scripted process tests
  for all four providers, fallbacks, completion repair, reviews, verification rework and the shared
  cap. Loop launches now decline project-port publication at the execution boundary while direct
  and ACP Serve opt-ins remain unchanged. Graduated this card to the canonical repository gate.
