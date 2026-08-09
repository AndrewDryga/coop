---
name: loop-provider-watchdog
description: built-in attempts always stream; the watchdog trusts only decoder events, and the box's own process group makes redirected loops handle stop signals themselves
subsystem: loop
sources: [internal/cli/watchdog.go, internal/cli/streamjson.go, internal/cli/streamjson_providers.go, internal/cli/commands.go, internal/box/run.go, internal/runtime/runtime.go]
updated: 2026-08-09
---

Every built-in loop/review/preflight attempt requests the provider's structured stream —
TTY or redirected — because the stream feeds the provider-attempt watchdog
(`internal/cli/watchdog.go`). Only valid adapter-recognized events reset it (the
`streamActivity` sink in streamjson.go): assistant/reasoning progress, tool start/end by
provider ID, terminal events. Raw bytes, malformed events, unknown types, redraws, `/proc`,
CPU, and lease heartbeats are deliberately NOT activity. All three deadlines DEFAULT TO 0 —
disabled, no timer at all (73d2634, "stop killing models that are still working"): a clock
cannot tell a long attempt from a wedged one, and killing a working provider loses its answer
and re-pays for the whole task. When set they are start (to the first model action; bootstrap
doesn't count), idle (post-progress silence, suspended while a foreground tool is open), and
tool (an absolute cap on the OLDEST open tool). Above them runs the non-resettable attempt
ceiling. Timeouts classify as `provider_{start,idle,tool,attempt}_timeout`, retry under their
own consecutive cap of 3, and rotate without cooling the abandoned rung.
`COOP_PROVIDER_TIMEOUTS` ("start=2s,idle=3s,tool=6s") is the internal test-only override, and
it may only shorten.

**The stream is the box's stdout, which makes this a trust boundary, not a protocol.** The bytes
come from the box that holds the credential and runs the agent — and any descendant in it that
reaches that descriptor can write provider-shaped NDJSON (the watchdog e2e proves it by having a
non-provider child flood the inherited stdout). So the events are treated as an untrusted input
with three layers, in this order:

1. **Semantic validity** — recognition alone is not enough; an event must carry the fields that
   make it mean something, or it produces no activity at all. Claude: an assistant turn needs a
   block with text/thinking/redacted-`data`/tool name, and a `tool_use` needs BOTH id and name to
   open a tool; a `tool_result` needs its `tool_use_id`. Codex: a recognized item kind AND a
   nonempty item id (that id is what every lifecycle event is keyed on). Gemini: nonempty
   `content` for an assistant delta, nonempty `tool_id` for tool_use/tool_result. Grok: nonempty
   `data` for text and thought. Empty-but-schema-valid envelopes were free deadline resets before
   2026-08-09.
2. **Bounded state** — everything the attempt retains per provider-supplied id is capped:
   `maxOpenTools` (64) in the watchdog, `maxStreamTrackedIDs` (256) for decoder labels and
   show-once markers, plus the pre-existing 1 MiB event / 64 KiB narration / 64 KiB tail bounds.
   Overflow is DROPPED, never evicted — evicting the oldest open tool would re-anchor the absolute
   tool cap to a younger one and sell an endless extension for one forged start per interval.
3. **The attempt ceiling** — the honest admission that 1 and 2 cannot make a stream truthful. A
   forged content-bearing event is indistinguishable from a real one, so a stream that only wants
   to stay alive can reset any phase deadline forever. The ceiling is armed once at the launch
   boundary and fires under generation 0, which no re-arm can retire.

Traps the code doesn't obviously carry:

- **The start deadline is armed by box.Run, not by its caller.** box.Run does substantial host
  work before it launches anything — filesystem projection, sibling services, network inspect,
  argument assembly — so the watchdog is built UNARMED and armed from `RunSpec.OnRuntimeLaunch`,
  box.Run's one callback at the runtime-launch boundary: after all host setup, immediately before
  the container starts, exactly once (`internal/box/run.go`). Arming any earlier both mislabels
  coop's own slow setup as `provider_start_timeout` AND cannot act on it, because cancelling the
  box context does nothing to synchronous setup that has not reached `RunInterruptible`. A run
  that fails before the boundary never arms, so it reports its real error instead of a timeout.
- **The box runs in its own process group for every built-in attempt** (non-nil Ctx →
  `RunInterruptible`, runtime.go). A signal delivered to coop's group no longer reaches the
  box. The loop therefore watches SIGINT/SIGTERM even without a TTY and cancels the box
  context (commands.go); killing coop with `-9` orphans the box — matching a real container
  outliving its client — and the crash e2e tests reap that orphan explicitly.
- **Raw stdout lines in a streaming attempt are terminal diagnostics**, not narration — a
  plain `error: rate limited` line WILL rotate targets. Only text inside decoded assistant
  events is quarantined narration. Fixtures simulating ambiguous prose must emit it as
  streamed narration.
- **The attempt ceiling is DERIVED (24 × the longest armed phase), never configured.** No env
  var, conf file, or event can lengthen it, and it is 0 — no timer at all — while the phases are
  disabled, so today it changes nothing. That inertness is the point: with no phase clock running
  there is no supervision to bypass, and coop's promise not to interrupt a working provider
  stands. It is also the one bound that is deliberately NOT `transport-bounds-do-not-abort-valid-work`
  material: it bounds a hostile stream's wall clock at a multiple no legitimate iteration reaches,
  not the volume of valid work.
- **`COOP_PROVIDER_TIMEOUTS` is clamped, not obeyed.** It may only SHORTEN (a disabled 0 default
  counts as infinite, so any finite value shortens it), it may not set anything under
  `minWatchdogDeadline` (1s — below that a deadline kills healthy providers at launch rather than
  supervising them), a phase it never names keeps its default untouched (including a disabled 0
  the floor would otherwise "raise"), a malformed field keeps every default, and every clamp or
  rejection is named on stderr once per process. `resolveWatchdogDeadlines` takes its defaults as
  an argument so the policy is tested against ARMED values while the shipped ones are still 0.
- **Grok's stream carries no tool lifecycle**, so a grok foreground gate longer than the
  idle deadline gets killed; the watchdog fixtures reject grok tool scenarios.
- Parent cancellation always wins over a watchdog fire (`ctx.Err()` guard in runIteration):
  an interrupted run stays `interrupted`, never a provider timeout.

## Changelog
- 2026-08-09 — the stream is documented as a TRUST BOUNDARY: semantic validity per provider,
  bounded per-attempt state, and the new non-resettable `provider_attempt_timeout` ceiling; plus
  the override's clamp policy. Verified against watchdog.go, streamjson.go, streamjson_providers.go.
- 2026-08-09 — arming moved to box.Run's `OnRuntimeLaunch` boundary (new trap, `internal/box/run.go`
  added to sources); also corrected the deadline paragraph, which still claimed the 10m/30m/2h
  values 73d2634 had already zeroed.
- 2026-08-02 — reverified while making decoded live presentation width-aware; display fitting is
  downstream of the unchanged trusted activity callbacks and stream byte bounds.
- 2026-07-26 — created with the watchdog implementation (verified against the sources above).
