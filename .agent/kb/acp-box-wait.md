---
name: acp-box-wait
description: how a filtered editor session stays connected while its project box cannot start, and why no turn runs meanwhile
subsystem: acp
sources: [internal/cli/acp_box_wait.go, internal/cli/acp_cmd.go, internal/cli/fork_acp.go, internal/acpproxy/proxy.go, internal/acpproxy/restarter.go, internal/acpctl/start_notice.go, internal/box/project_preparation.go, internal/box/run.go]
updated: 2026-11-03
---

A filtered `coop acp` no longer exits when its project box cannot start. Admission runs with
`box.DeferProjectBuild`, and the supervisor asks `box.EditorBoxReadiness` itself: a project build miss
(`*ProjectBuildRequired`) or services with no rules (`*ServicesNeedRules`). Any other error is a failure
to look, and proves nothing.

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
- 2026-11-03 — created with the wait.
