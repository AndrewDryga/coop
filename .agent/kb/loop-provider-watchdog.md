---
name: loop-provider-watchdog
description: built-in attempts always stream; the watchdog is ARMED by default (10m/30m/2h) and trusts only decoder events, and the box's own process group makes redirected loops handle stop signals themselves
subsystem: loop
sources: [internal/cli/watchdog.go, internal/cli/streamjson.go, internal/cli/streamjson_providers.go, internal/cli/commands.go, internal/cli/ratelimit.go, internal/agent/agent.go, internal/agent/grok.go, internal/box/run.go, internal/runtime/runtime.go]
updated: 2026-08-09
---

Every built-in loop/review/preflight attempt requests the provider's structured stream —
TTY or redirected — because the stream feeds the provider-attempt watchdog
(`internal/cli/watchdog.go`). Only valid adapter-recognized events reset it (the
`streamActivity` sink in streamjson.go): assistant/reasoning progress, tool start/end by
provider ID, terminal events. Raw bytes, malformed events, unknown types, redraws, `/proc`,
CPU, and lease heartbeats are deliberately NOT activity. All three deadlines are ARMED BY
DEFAULT — start 10m (to the first model action; bootstrap doesn't count), idle 30m
(post-progress silence, suspended while a foreground tool is open), tool 2h (an absolute cap on
the OLDEST open tool) — and WHICH of them a provider runs is selected from its adapter's declared
stream capability, not from anything measured at runtime (trap below). Above them runs the
non-resettable attempt ceiling (48h at these values). Timeouts classify as
`provider_{start,idle,tool,attempt}_timeout`, retry under their own consecutive cap of 3, and
rotate without cooling the abandoned rung; the warning names the deadline AND the silence
observed (`providerWatchdog.timeoutDiagnostic`, carried to the retry sites in
`iterationClassification.detail` — telemetry still records the bare outcome).
`COOP_PROVIDER_TIMEOUTS` ("start=2s,idle=3s,tool=6s") is the internal test-only override, and
it may only shorten.

The zeroes those constants held between 73d2634 (2026-08-02) and 2026-08-09 were a real
correction, not an accident: a clock cannot tell a long attempt from a wedged one, and killing a
working provider loses its answer and re-pays for the whole task. They are armed again because the
opposite failure is unbounded — every retry budget in the loop counts outcomes that already
HAPPENED, so an attempt that never finishes is bounded by nothing, and one wedged provider CLI
holds an overnight drain, its task lease, and its credential until a human notices. What makes
arming safe is the three tasks that landed first: arming at the runtime-launch boundary, semantic
activity with bounded state and the non-resettable ceiling, and per-adapter policy for a stream
with no tool lifecycle. The values are silence budgets no honest attempt reaches, NOT
service-level targets — tighten one and the false-kill cost above comes straight back.

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
  coop's own slow setup as `provider_start_timeout` AND cannot act on it in time. Canceling the box
  context now aborts pre-launch setup too, but only at the NEXT step boundary: `ctxStep` checks
  between each of the four named phases above, never inside one — a wedged `compose up` or
  `network inspect` runs via plain `exec.Command` with no context of its own, so the cancellation
  lands once that ONE call returns, not before. A run canceled before the boundary — at entry or
  mid-setup — never arms the watchdog and never launches the container; a run that fails before the
  boundary for any other reason never arms either, so it reports its real error instead of a
  timeout.
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
  var, conf file, or event can lengthen it; a clamped override shortens it, and a policy that turns
  every phase off takes the ceiling with it. At the shipped values that is 48h. It is the one bound
  deliberately NOT `transport-bounds-do-not-abort-valid-work` material: it bounds a hostile stream's
  wall clock at a multiple no legitimate iteration reaches, not the volume of valid work.
- **The idle deadline and the box's descendant drain are one coupled pair.** A draining box emits
  no stream events, so `COOP_DESCENDANT_TIMEOUT` (900s in `internal/box/image.go`) and the 30m idle
  deadline run from the same instant; if the drain could reach the deadline, a box held open by a
  leaked descendant would die as a wedged provider and the drain's own exit codes would never be
  observed. `TestShippedProviderDeadlinesAreArmed` reads the drain default out of
  `box.BaseDockerfile()` and fails if idle drops below 2× it — check the test, not the prose, before
  moving either. (image.go's own comment still says the deadline is disabled by default: its text is
  sha256-stamped into the box image, so editing a comment there marks every built image stale.)
- **`COOP_PROVIDER_TIMEOUTS` is clamped, not obeyed.** It may only SHORTEN (a disabled 0 default
  counts as infinite, so any finite value shortens it), it may not set anything under
  `minWatchdogDeadline` (1s — below that a deadline kills healthy providers at launch rather than
  supervising them), a phase it never names keeps its default untouched (including a disabled 0
  the floor would otherwise "raise"), a malformed field keeps every default, and every clamp or
  rejection is named on stderr once per process — that announcement is also how the e2e proves the
  UNNAMED phases still carry the shipped defaults, which is the only way to see a 10m deadline in a
  test that finishes in seconds. `resolveWatchdogDeadlines` takes its defaults as an argument so the
  clamp policy is tested against fixed values, not whatever the shipped constants currently are.
- **Policy comes from the adapter's DECLARED stream capability, never from measurement.**
  `agents.StreamSpec.ToolLifecycle` is `ToolLifecycleIDs` for claude/codex/gemini and
  `ToolLifecycleAbsent` for grok, whose streaming-json emits only thought/text/end (probed at
  v0.2.101). `providerWatchdogPolicy` turns that into supervision: IDs keep idle+tool exactly as
  shipped; absent trades BOTH for one post-progress deadline at `providerSilenceFallbackMultiple`
  (4) × idle — 30m idle → a 2h fallback — with no tool cap, because a gate that never appears in
  the stream is indistinguishable from silence and the ordinary idle deadline would kill it. It is
  derived from idle rather than a 2h constant, so the shorten-only override shortens it too (and a
  fixture can exercise it in seconds), and a disabled idle turns it off too. The ceiling stays
  outermost: 24 × the longest phase, which is now the fallback. The watchdog also REFUSES tool
  events from a stream that declared none — nothing may suspend a deadline whose resuming event
  does not exist. `ToolLifecycleUndeclared` (the zero value) reads as absent and fails
  `TestEveryStreamDeclaresItsToolLifecycle`, so no stream ships unprobed. Fixtures still reject
  grok TOOL scenarios; a grok long gate is scripted as `progress-gated-complete`.
- Parent cancellation always wins over a watchdog fire (`ctx.Err()` guard in runIteration):
  an interrupted run stays `interrupted`, never a provider timeout.

## Changelog
- 2026-08-09 — pre-launch setup is now itself step-boundary cancelable (`ctxStep` in
  `internal/box/run.go`, one check between each of the four named phases, plus one at entry).
  Corrected the arming trap above, which had claimed canceling the box context does nothing until
  `RunInterruptible` — true when it was written, and the gap this same trap flagged; a canceled
  setup now aborts promptly at the next boundary and still never arms the watchdog.
- 2026-08-09 — the deadlines are ARMED by default again (10m/30m/2h, ceiling 48h): rewrote the
  opening paragraph, the ceiling and override traps, and added the idle-vs-descendant-drain
  coupling trap plus the observed-silence diagnostic. The e2e proof of default-on is an override
  naming ONE phase and asserting the announcement's UNNAMED phases.
- 2026-08-09 — supervision is now selected from the adapter's declared stream capability
  (`internal/agent/agent.go` + `grok.go` added to sources); the admitted contradiction — "a grok
  foreground gate longer than the idle deadline gets killed" while the docs promised long gates
  survive — is resolved by the derived no-tool-lifecycle fallback, not by documentation.
- 2026-08-09 — the stream is documented as a TRUST BOUNDARY: semantic validity per provider,
  bounded per-attempt state, and the new non-resettable `provider_attempt_timeout` ceiling; plus
  the override's clamp policy. Verified against watchdog.go, streamjson.go, streamjson_providers.go.
- 2026-08-09 — arming moved to box.Run's `OnRuntimeLaunch` boundary (new trap, `internal/box/run.go`
  added to sources); also corrected the deadline paragraph, which still claimed the 10m/30m/2h
  values 73d2634 had already zeroed.
- 2026-08-02 — reverified while making decoded live presentation width-aware; display fitting is
  downstream of the unchanged trusted activity callbacks and stream byte bounds.
- 2026-07-26 — created with the watchdog implementation (verified against the sources above).
