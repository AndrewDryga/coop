---
name: loop-provider-watchdog
description: built-in attempts always stream; the watchdog trusts only decoder events, and the box's own process group makes redirected loops handle stop signals themselves
subsystem: loop
sources: [internal/cli/watchdog.go, internal/cli/streamjson.go, internal/cli/commands.go, internal/runtime/runtime.go]
updated: 2026-07-26
---

Every built-in loop/review/preflight attempt requests the provider's structured stream —
TTY or redirected — because the stream feeds the provider-attempt watchdog
(`internal/cli/watchdog.go`). Only valid adapter-recognized events reset it (the
`streamActivity` sink in streamjson.go): assistant/reasoning progress, tool start/end by
provider ID, terminal events. Raw bytes, malformed events, unknown types, redraws, `/proc`,
CPU, and lease heartbeats are deliberately NOT activity. Deadlines: 10m to first model
action (bootstrap doesn't count), 30m post-progress silence (suspended while a foreground
tool is open), 2h cap on the oldest open tool. Timeouts classify as
`provider_{start,idle,tool}_timeout`, retry under their own consecutive cap of 3, and rotate
without cooling the abandoned rung. `COOP_PROVIDER_TIMEOUTS` ("start=2s,idle=3s,tool=6s") is
the internal test-only override.

Traps the code doesn't obviously carry:

- **The box runs in its own process group for every built-in attempt** (non-nil Ctx →
  `RunInterruptible`, runtime.go). A signal delivered to coop's group no longer reaches the
  box. The loop therefore watches SIGINT/SIGTERM even without a TTY and cancels the box
  context (commands.go); killing coop with `-9` orphans the box — matching a real container
  outliving its client — and the crash e2e tests reap that orphan explicitly.
- **Raw stdout lines in a streaming attempt are terminal diagnostics**, not narration — a
  plain `error: rate limited` line WILL rotate targets. Only text inside decoded assistant
  events is quarantined narration. Fixtures simulating ambiguous prose must emit it as
  streamed narration.
- **Grok's stream carries no tool lifecycle**, so a grok foreground gate longer than the
  idle deadline gets killed; the watchdog fixtures reject grok tool scenarios.
- Parent cancellation always wins over a watchdog fire (`ctx.Err()` guard in runIteration):
  an interrupted run stays `interrupted`, never a provider timeout.

## Changelog
- 2026-07-26 — created with the watchdog implementation (verified against the sources above).
