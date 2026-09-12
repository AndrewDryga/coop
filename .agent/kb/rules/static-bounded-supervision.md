---
name: static-bounded-supervision
description: "supervised long commands keep output static and only a bounded excerpt in context"
scope: agent-workflow
sources: [AGENTS.md, internal/loop/bar.go, internal/loop/iteration.go, internal/ui/live.go]
check: "go test ./internal/loop -run TestLoopBarSupported"
updated: 2026-09-12
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
  report the command's real exit status.
- While it runs, poll for completion without streaming the log. Inspect only a bounded `tail`
  or a targeted `rg` filter, expanding narrowly when a failure needs more evidence.
- Do not dump the complete log into model context, pipe a live command through `tail`, or disable
  the interactive UI in product code to satisfy this supervision rule.
- Verify human output separately: readable task/provider/account identity, distinct sections,
  truthful waits/retries/recovery and final results. Normal human terminals keep their live UI.

See also [[command-output-tiers]] and [[fix-the-bug-not-the-feature]].

## Changelog
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
