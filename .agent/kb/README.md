# .agent/kb — the self-improving knowledge base

Descriptive operational knowledge an agent needs but the code doesn't obviously carry: subsystem
maps, cross-cutting traps, hard-won gotchas. A card directly under `kb/` is DESCRIPTIVE ("here's how
X actually works, and the trap").

The NORMATIVE floor lives one level down in **[`rules/`](rules/README.md)** ("do X, not Y") — a
verdict a review can fail against, with its own index, card format, and `check:` field. Read that
index at boot too; a rule may link up to a card here for background. One committed knowledge tree,
two registers.

## Reading protocol
Read this INDEX at boot; open a card ONLY when your task touches its subsystem. Never bulk-load the
kb into a prompt — the index is the routing table, cards are pulled on demand (like skills). That
scoping is also the safety rail: a card only ever reaches the prompts of tasks in its own subsystem,
so a wrong card can't poison work it doesn't touch.

## You maintain this KB — directly
This is a self-improving wiki: no inbox, no human gate. When a task teaches you something non-obvious
about a subsystem — a map, a trap, a gotcha the code doesn't carry — CREATE or UPDATE its card here,
in the same commit as the work. Keep it TIDY as it grows: once a flat list gets long, group cards
into per-subsystem subfolders (`box/`, `acp/`, `loop/`…) and keep this index current. The structure
is yours to evolve — organize it however keeps it usable.

The discipline that replaces the human gate is the metadata: every card states when it was last
`updated`, which `subsystem` it maps, and the `sources` (the code) it describes — so staleness is
obvious at a glance. When you pass through a subsystem, check its cards against their `sources`; if
one has drifted, re-verify and bump it (with a changelog line) or DELETE it — a card that
contradicts the code is worse than no card.

## Card format
One fact per file: frontmatter, a short body (under a screen), and a small changelog so an outdated
card is obvious.

```
---
name: <kebab-case-slug>              # = the filename
description: <one line — judged for relevance straight from this index>
subsystem: <box | acp | loop | …>    # groups the index; decides when the card loads
sources: [internal/box/run.go, …]    # the code this describes — check drift against it
updated: <YYYY-MM-DD>                # last edit
---

<the fact; cite file:line for load-bearing claims; link related cards with [[name]]>

## Changelog
- <YYYY-MM-DD> — created / what changed (and what you verified it against)
```

`make rules-check` holds you to the mechanical half of that — every field present and non-empty,
`name` matching the filename, `updated` a real date, every `sources:` path still existing, a
changelog section, and Index pairing BOTH ways (every card indexed exactly once, every indexed link
resolving). What it can't tell you is whether a card is still TRUE; only reading it against its
`sources` does that.

## Index
- [box-time-is-utc](box-time-is-utc.md) — boxes run UTC; the host TZ is forwarded so rate-limit reset prose parses back host-local
- [box-home-nested-mounts](box-home-nested-mounts.md) — avoid bind targets that make Docker create missing application-owned home parents as root
- [box-entrypoint-descendant-handoff](box-entrypoint-descendant-handoff.md) — supervised loop/review boxes authenticate forwarder exemptions and hand off live detached jobs
- [box-orphans-survive-pdeathsig](box-orphans-survive-pdeathsig.md) — Pdeathsig is not inherited across fork, so a forking worker orphans into the box and holds it open for the whole drain
- [box-supervisor-label-and-orphan-sweep](box-supervisor-label-and-orphan-sweep.md) — every box records the host process supervising it; only a provably dead one authorizes a reap, scoped to the workspace that launched it
- [box-host-port-window-is-contended](box-host-port-window-is-contended.md) — coop's deterministic serve host ports sit inside the OS ephemeral range, so any process can be holding one; publishing is best-effort and a test must never claim one for real
- [host-disk-exhaustion-stops-the-runtime](host-disk-exhaustion-stops-the-runtime.md) — a host out of disk OR memory stops the container runtime mid-run and surfaces as unexplained "unexpected EOF" failures; prune alone never returns the space
- [services-teardown-needs-the-workspace](services-teardown-needs-the-workspace.md) — sibling services persist across boxes on purpose, but teardown reads the workspace's compose file, so stopping them after deleting the workspace does nothing
- [credentials-expired-is-a-false-alarm](credentials-expired-is-a-false-alarm.md) — refreshable OAuth stays signed in; re-login required means the stored login cannot recover
- [credential-presence-is-adapter-declared](credential-presence-is-adapter-declared.md) — adapters own credential presence, selected env authority, and inspectable stored readiness
- [provider-scripted-e2e](provider-scripted-e2e.md) — drive the external Coop CLI through strict runtime/provider fixtures without ambient state
- [provider-live-e2e](provider-live-e2e.md) — probe installed upstream CLIs with isolated read-only, native-resume, and task-completion workflows
- [provider-session-history](provider-session-history.md) — native session layouts, lookup bounds, and the large-history regression contract
- [provider-consult-e2e](provider-consult-e2e.md) — verify generated coop-consult behavior through all provider arms, fallback pairs, and a four-edge live ring
- [model-tiers-and-role-vs-lead](model-tiers-and-role-vs-lead.md) — ModelFor is one model per provider (active>target>fallback>env); a preset role's model rides its wrapper target, never global state, or it shadows a rotated lead
- [loop-live-bar](loop-live-bar.md) — the loop's sticky bottom bar; every paint parks the cursor at column 0 so a kernel ^C echo can't wrap the line and desync the region's erase math — the subsystem once deleted instead of debugged
- [loop-provider-watchdog](loop-provider-watchdog.md) — built-in attempts always stream; the watchdog is ARMED by default (10m/30m/2h) and trusts only decoder events, and the box's own process group makes redirected loops handle stop signals themselves
- [loop-rotation-advance-triggers](loop-rotation-advance-triggers.md) — rate limits cool and come back, auth failures are sticky for the run; known-invalid credentials never become rungs
- [loop-resume-never-rewrites-history](loop-resume-never-rewrites-history.md) — a leaked box descendant un-completes committed work; resuming it must never amend a non-HEAD commit
- [loop-range-rejects-outside-commits](loop-range-rejects-outside-commits.md) — an iteration's commit range is a time window: a foreign commit's Coop-Task trailer rejects that iteration's completion only when it names a task this iteration's authority could touch, else it's tolerated and journaled; the per-worktree ref-authority lock closes the narrower validate-then-consume race
- [fork-review-scratch-two-copies](fork-review-scratch-two-copies.md) — forkctl and sessionsvc keep separate review-scratch clones on purpose: the same 18-line scaffold under opposite anchoring contracts (live HEAD preview vs verified captured intent)
- [fork-lifecycle-state-file](fork-lifecycle-state-file.md) — one pidfile holds four fork lifecycle states; only pid+start-token (never file age) may declare its owner gone, and only a pre-fork reservation is reclaimable
- [acp-preset-owns-toolbar](acp-preset-owns-toolbar.md) — active ACP presets own the whole lead target and refuse stale Provider/Account editor replays
- [acp-auth-is-provider-account-scoped](acp-auth-is-provider-account-scoped.md) — initialize capability truth and successful authentication belong to one provider account
- [acp-scripted-e2e](acp-scripted-e2e.md) — test the real ACP supervisor/control/proxy path with a scripted runtime and isolated state
- [acp-replay-publication](acp-replay-publication.md) — publish replacement native bindings atomically before releasing held editor work
- [acp-target-commit](acp-target-commit.md) — commit model/effort truth from the effective provider response, including Grok migrations
- [acp-carry-echo](acp-carry-echo.md) — inject best-effort context once and hide only its exact provider echo from the editor
- [acp-generated-output-boundary](acp-generated-output-boundary.md) — generated images bypass transcript bytes but remain bounded, immutable turn artifacts
- [acp-rewrites-must-keep-line-framing](acp-rewrites-must-keep-line-framing.md) — an ACP line rewrite that drops the trailing newline hangs the session silently; parsing tests still pass
- [codex-acp-agent-mode](codex-acp-agent-mode.md) — Codex ACP must select full-access mode explicitly because session config does not override its per-turn sandbox policy
- [session-api-dto-is-a-second-projection](session-api-dto-is-a-second-projection.md) — a field on the durable session record stays invisible to API clients until the hand-written DTO and its public* copier carry it too
- [session-operation-intents-cross-versions](session-operation-intents-cross-versions.md) — running session operations survive binary upgrades, so persisted intent JSON needs explicit compatibility normalization before replay
- [session-companions-own-git-metadata](session-companions-own-git-metadata.md) — companion boxes mount only the pinned workspace, so each snapshot must own usable Git metadata rather than point back to its source checkout
- [review-host-owned-verdicts](review-host-owned-verdicts.md) — review boxes report bounded evidence; Coop alone applies validated task lifecycle changes
- [signoff-scope-is-run-anchored](signoff-scope-is-run-anchored.md) — signoff subjects are a run-anchored folder diff; re-anchor only on receipt-consistent rounds, from the post-review done set
- [task-state-is-the-folder](task-state-is-the-folder.md) — a task's state IS its directory; a bare `mv` to a missing state dir silently corrupts the queue
- [task-authority-registry-is-durable-state](task-authority-registry-is-durable-state.md) — host-global completion trust lives in `~/.local/state/coop/task-leases`, never a cache dir; v9 refuses populated retired state and every authority flock rechecks its inode
- [task-authority-model](task-authority-model.md) — four separate authorities decide who may act on a task/checkout: claim (durable, human-released), lease, checkout, and ref (all three kernel flocks); never conflate them
- [task-tmp-lifetime](task-tmp-lifetime.md) — task-local tmp survives resumable states but is containment-cleaned on done before review; artifacts persist
