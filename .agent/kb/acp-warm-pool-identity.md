---
name: acp-warm-pool-identity
description: coop acp parks one box per signed-in provider the session is not using and lends it to a switch only on an identical identity — plain target at the default model/effort, same account, same box image; prove a hit from the trace, never from timing
subsystem: acp
sources: [internal/acpctl/warm.go, internal/acpctl/resume.go, internal/acpctl/control.go, internal/cli/acp_cmd.go, internal/acpproxy/proxy.go, internal/runtime/runtime.go, internal/box/derived_image.go, tools/lifecycle_bench.py]
updated: 2026-09-22
---
`coop acp` keeps a parked box (`acpctl.WarmPool`) for each signed-in provider the session is not
using. After every factory spawn — the first one included, which is what fans the pool out —
`WarmPool.Rebalance` warms the others (the provider just left among them) and stops any spare of the
provider now active, so the idle set stays one box per other provider.

A switch takes the parked box only when it would start the same box cold:
- `acpctl.WarmSwitch`: a plain target (no preset) at the provider's default model and effort — what a
  parked box runs. The Provider selector always resolves an account (Auto → the provider's first
  signed-in one), so the account cannot be part of that test; before 2026-09-19 the pool required a
  bare `Target{Provider}` and therefore NEVER served an editor switch on a host with accounts.
- `WarmPool.Checkout(provider, account, image)`: the parked child's `Account` must equal the target's,
  and its recorded `Image` must equal the box image a cold start would use now (`Runtime.ImageID` of
  `box.ImageForRepo` for ordinary runs, `box.FilteredImageIdentity` for filtered runs). A box from a replaced image is stopped (a `coop build` mid-session); an
  unknown image ("", e.g. Apple container) reuses nothing; a box on another account stays parked.
Why these and not more: the workspace, network capture, MCP config and roles are the supervisor's
own and the same for both spawns.
`spawnBox` strips ambient `COOP_EGRESS` and exports the resolved explicit config value to every
initial, replacement and warm child. Admission marks that value explicit; inner children skip
admission and must not lose a one-off or remembered offline decision. Filtered mode still needs
its independently authenticated capture and account bindings.
Credentials are live profile mounts the client refreshes itself; in a filtered session a checkout first
passes `acpFilteredLaunchProof` — the same re-proof (scope, admission, auth family, portability) a cold
launch runs after its reset wait — so reuse passes the same credential gate as a cold start. Grok
carries its model in the launch command, so a model mismatch could not be fixed after checkout —
hence exact matching. A filtered key names the capture's pinned client image or the exact explicitly
built project image. Changed build-context inputs, absent approval or a missing image prevent reuse;
a new cold launch still performs the full layer/client proofs.

A warm spawn never waits: on an account still waiting out a rate limit (`Control.Cooling`) it is refused
and the slot stays empty, because `Reap` — editor close, SIGHUP reload — waits for every spawn in flight
and for any eviction's stop. The pool warms the others once the editor's initialize has been ANSWERED — not
while the lead's own box is starting — and again after every later spawn; it is off when the image's
id cannot be read (Apple container): no box could be proved current, so none would be lent. Parked boxes resolve their account like the selector's Auto
(`ResolveNetworkTarget`) in open and filtered sessions alike. Filling used to start beside the lead, and that cost the editor ~0.23 s (~40%) on its
`initialize` — not in coop's own startup work, which measured identical either way, but in the lead's
adapter booting while three more containers started. It is deferred behind `Hooks.Initialized` (with
a 3 s fallback, so a lead that never answers still leaves the others ready), measured back at
-0.268 s over 6 paired rounds with the warm hit rate intact (3/3). The lesson generalizes: anything
only a LATER request needs must not run beside the one an editor blocks on. See
[[lifecycle-latency-measurement]] for the paired method.

**Evidence, not timing.** With `COOP_ACP_TRACE=1` the trace says `spawn: warm box for P@A` or
`spawn: cold box for P@A` for each spawn, `warm pool: P@A parked` when a box parks (started, not
proven up — a parked box that died is found by the replay after checkout, which starts another cold),
and `replay: P@A is
live on <adapter> <version>` once a replayed session runs. `tools/lifecycle_bench.py --cases
acp_switch_cold,acp_switch_warm` times a Provider switch to the replayed `config_option_update` and
counts a warm sample only on `spawn: warm box`; `TestScriptedACPProviderSwitchIsServedWarm` proves the
wiring on the fixture runtime. The bench parses these lines, so change them together.

**Stop.** When the proxy begins shutting down it calls `RunOpts.Stopping` before stopping the active
child; the supervisor then cancels in-flight warm fills (a fill that has not launched launches nothing)
and reaps the pool, whose parked boxes stop concurrently — beside the active box, not after it. The
deferred reap waits for that same teardown. Measured on a filtered project with two parked boxes:
editor close → everything gone in ~1.4 s.

## Changelog
- 2026-09-22 — recorded the shared child-mode handoff, verified by actual spawned-process
  offline/open/conflicting-environment regressions, including standalone-probe controls.
- 2026-09-22 — filtered warming checks the actual explicitly built image and its current approved
  inputs; removed the unrelated ordinary-image prerequisite.
- 2026-09-20 — recorded the measured cost of filling on the editor handshake, with the
  measurement card that owns the evidence.
- 2026-09-19 — stop: Stopping hook, cancelled fills, concurrent reap
- 2026-09-19 — rewritten from acp-warm-pool-serves-only-bare-targets: identity-matched reuse, image
  check, rebalance; the measured miss it replaced is recorded in task 2026-09-18-measure-editor-warm-…
