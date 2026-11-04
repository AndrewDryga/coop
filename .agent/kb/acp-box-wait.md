---
name: acp-box-wait
description: how a filtered editor session stays connected while its project box cannot start, and why no turn runs meanwhile
subsystem: acp
sources: [internal/cli/acp_box_wait.go, internal/cli/acp_cmd.go, internal/cli/acp_refresh.go, internal/cli/fork_acp.go, internal/acpproxy/proxy.go, internal/acpproxy/restarter.go, internal/acpctl/start_notice.go, internal/box/project_preparation.go, internal/box/run.go, internal/box/network_admission.go, internal/box/filtered.go, internal/box/filtered_launch.go, internal/box/authority_mounts.go]
updated: 2026-11-03
---

A filtered `coop acp` no longer exits when its project box cannot start. Admission runs with
`box.DeferProjectBuild`, and the supervisor asks `box.EditorBoxReadiness` itself: a project build miss
(`*ProjectBuildRequired`), services with no rules (`*ServicesNeedRules`) or rules nobody approved
(`*networkstate.PendingApproval`). Any other error is a failure to look, and proves nothing.

Rules nobody approved come first, since no capture exists for them to run under. `HoldPending` admits
`holdingAdmission`: the person's flags, the host's MCP and the provider bundles, and nothing from the
project's file, approved or not (a service grant would start sidecars). `CapturedEgress.Pending` marks
it, `spawnBox` then holds prompts whatever the wait last heard, and only a real capture ends the wait:
`acpNetworkRefresh` adopts one when the generation moves, and `readmitHeld` tries on every wait check,
since taking a request back moves no generation. A withdrawal, a replaced folder and an offline mode
still refuse at connect, since the store refuses them or filtered would widen them. Fork ACP does not
ask for this: it has no refresh to end the wait, so it still refuses at connect. An approved open or
offline mode cannot be moved to (`errACPNetworkModeChanged`), so the thread says to reconnect.

Trap: a first approval takes `LockServiceLaunch` exclusively, and every box holds the shared side for
its lifetime, so a holding child would deadlock the approval it waits for. A holding child therefore
sees the project read-only, passes the mount guard as if anchored (`authorityProtection`), and drops
the window once validated. A real box in the project still makes a first approval wait, in silence.

While blocked, `spawnBox` starts a child with `COOP_ACP_HANDSHAKE_BOX=1`: `RunSpec.HandshakeOnly` runs
the locked client image on the same admitted policy, without serve ports or the join-all service
network, and `Child.HoldPrompts` tells the proxy no prompt may reach it. Everything else does reach it:
`initialize`, `session/new`, `session/load` and settings need a real adapter, which is why this is a
real box and not a fake answer. Prompts go into `restartHeld`, the queue a provider switch uses, so
cancel, duplicate and size limits are the existing ones. They pass through `FromEditor` only at
release. A resend stays reserved: `ResumePrompt` is not fed to a holding child, and a reserved id does
not stop the move (`movableOffHoldingLocked`). A replacement that dies during replay leaves held and
reserved prompts for the next child; `failAllPending` answers them if the proxy gives up.

`acpBoxWait` polls every 2 s. Asking the real question reads the build context, about 1.1 s on a 5,600
entry project, so it asks only when `EditorBoxStamp` changes (the review record, which every finished
project build rewrites; project.yaml; the Dockerfile; the policy fingerprint) or every 30 s, which is
what catches an undone edit to a copied file. Approved files rebuild once in the background, with the
wait's own context, never a factory's. A failed rebuild is not retried until a check clears.

Trap: a held prompt makes `Restarter.Idle` false, and the network refresh waits for idle to adopt an
approval. The services wait is settled by `coop approve`, so refresh also counts `Holding` as idle: no
turn is running. The move itself is the wait's job, through `MoveOffHolding`, since `RestartIfIdle`
would never fire with a prompt held.

Trap: warm boxes. A fill while blocked would park a box that can serve no switch, so fills return
`errACPBoxWaiting` and `boxOpened` starts the fan-out when the session clears. The warm-off check at
startup skips a blocked session, or the pool stays off for its whole life.

Fork ACP shares the wait, but its hooks have no start notice, so a thread hears about it only when it
sends a message. See [[restricted-project-builds-are-explicit]].

Limit: readiness asks about project builds and services with no rules only. Any other refusal (an
approved service whose definition changed, a serve port the policy grants TLS on) still ends in the
proxy's rapid-fail cap, and a holding child still starts approved sidecars, so a broken one kills it too.

## Changelog
- 2026-11-03 — rules nobody approved wait too, on a holding capture. A real run on Docker hung `coop
  approve` on the lifetime lock, which the read-only mount and the dropped window fixed.
- 2026-11-03 — created with the wait.
