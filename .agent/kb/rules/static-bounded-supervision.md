---
name: static-bounded-supervision
description: "supervised long commands keep output static and only a bounded excerpt in context"
scope: agent-workflow
sources: [AGENTS.md, internal/loop/bar.go, internal/loop/iteration.go, internal/ui/live.go, internal/loop/prompts.go, internal/loop/shell_recipe_test.go]
check: "go test ./internal/loop -run 'TestLoopBarSupported|TestLoopCheckScript|TestLoopShellGuidance'"
updated: 2026-09-14
---

# Agent-supervised long commands use static, bounded output

When an agent supervises a long loop, gate, watch, build, or test command, keep the command's
terminal output static and keep only a bounded excerpt in model context. This is an agent
execution rule, not a product change: human-run interactive commands keep their live UI.

**Why:** streaming a repainting progress view into an agent transcript repeats whole terminal
frames, consumes context without adding evidence, and can obscure the final exit status.

**How to apply:**
- Set `TERM=dumb`, `NO_COLOR=1`, and `COOP_SPINNER=0` where the command supports them.
- Turn the progress region OFF, not merely its animation. `COOP_SPINNER=0` freezes the spinner;
  non-terminal output or `TERM=dumb` disables the loop bar. Use the latter when a real PTY is
  required to test the user's invocation without feeding cursor repaints into model context.
- Redirect complete stdout and stderr to the task's `tmp/` directory or `/tmp`; preserve and
  report the command's real exit status. Establish the check's explicit cwd, create scratch
  before redirection, and use a unique log per invocation so concurrent checks retain evidence.
- Run final checks in the foreground by default. Background a check only when there is
  independent work to do; a returned running-job identifier is waited on through the runtime's
  completion tool, not a guessed shell sleep. If that wait times out, wait on the same job again.
  Inspect only a bounded `tail` or targeted `rg` filter, expanding when a failure needs evidence.
- Use the runtime's supported completion mechanism, not long blind sleeps. Cleanup targets only
  verified owned resources through their lifecycle command or exact identity, then checks the
  stopped/absent postcondition; a cleanup failure remains a failure, not a successful tail.
- Do not dump the complete log into model context, pipe a live command through `tail`, or disable
  the interactive UI in product code to satisfy this supervision rule.
- Verify human output separately: readable task/provider/account identity, distinct sections,
  truthful waits/retries/recovery and final results. Normal human terminals keep their live UI.

See also [[command-output-tiers]] and [[fix-the-bug-not-the-feature]].

## Changelog
- 2026-09-14 — the user approved foreground-first checks after the Opus run kept using
  guessed sleeps despite generic wait guidance. Swept the work prompt, shell guidance tests,
  and supervision rule; replaced the existing wording and pinned both normal/rework prompts.
  This is prompt guidance, not runtime enforcement or proof of model compliance.
- 2026-09-13 — swept the work prompt, loop bar/iteration and shared spinner: retained human UI,
  repaired missing setup/status/owned-job guidance with one executable POSIX check recipe.
  Focused tests cover cwd/setup refusal, argv boundaries, complete unique logs and producer failure
  despite a successful tail. Synthetic cleanup tests do not prove native model or DB compliance.
- 2026-09-12 — user reiterated no progress bar during supervision and informative human output.
  Swept loop bar selection, its iteration caller and shared spinner control: spinner-off retained
  the region and TERM=dumb was ignored. Fixed the loop's TERM capability check; regression proves
  dumb terminals/pipes stay static and normal interactive terminals retain the bar. Log bounding
  and semantic readability remain review obligations rather than claims made by this unit test.
- 2026-07-26 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: read AGENTS.md (its only source) in full. 0 violations
  — AGENTS.md:18 states this rule nearly verbatim and explicitly names this card by path ("follow
  `.agent/kb/rules/static-bounded-supervision.md`: disable repainting where supported, redirect
  the full log, preserve the exit status, and inspect only bounded tails or targeted filters"), a
  direct, current, word-for-word match.
