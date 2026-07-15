# .agent/kb — the self-improving knowledge base

Descriptive operational knowledge an agent needs but the code doesn't obviously carry: subsystem
maps, cross-cutting traps, hard-won gotchas. Sibling of `rules/` — but `rules/` is NORMATIVE
("do X, not Y") while a card here is DESCRIPTIVE ("here's how X actually works, and the trap"). A
rule may link to a card for background.

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

## Index
- [box-time-is-utc](box-time-is-utc.md) — boxes run UTC; the host TZ is forwarded so rate-limit reset prose parses back host-local
- [credentials-expired-is-a-false-alarm](credentials-expired-is-a-false-alarm.md) — claude "token expired" still works in-box via the refresh token
- [credential-presence-is-adapter-declared](credential-presence-is-adapter-declared.md) — file, primary, alternate, and env-only credential truth converges in the box predicate
- [provider-scripted-e2e](provider-scripted-e2e.md) — drive the external Coop CLI through strict runtime/provider fixtures without ambient state
- [provider-live-e2e](provider-live-e2e.md) — probe installed upstream CLIs with one isolated read-only marker request and stable pass/skip/fail evidence
- [model-tiers-and-role-vs-lead](model-tiers-and-role-vs-lead.md) — ModelFor is one model per provider (active>target>fallback>env); a preset role's model rides its wrapper target, never global state, or it shadows a rotated lead
- [acp-preset-owns-toolbar](acp-preset-owns-toolbar.md) — active ACP presets own the whole lead target and refuse stale Provider/Account editor replays
- [acp-auth-is-provider-account-scoped](acp-auth-is-provider-account-scoped.md) — initialize capability truth and successful authentication belong to one provider account
- [acp-scripted-e2e](acp-scripted-e2e.md) — test the real ACP supervisor/control/proxy path with a scripted runtime and isolated state
- [acp-replay-publication](acp-replay-publication.md) — publish replacement native bindings atomically before releasing held editor work
- [acp-target-commit](acp-target-commit.md) — commit model/effort truth from the effective provider response, including Grok migrations
- [acp-carry-echo](acp-carry-echo.md) — inject best-effort context once and hide only its exact provider echo from the editor
- [review-task-only-mounts](review-task-only-mounts.md) — review boxes bind the repo read-only, then remount validated task-queue descendants writable; their order is the isolation boundary
- [signoff-scope-is-run-anchored](signoff-scope-is-run-anchored.md) — signoff subjects are a run-anchored folder diff; re-anchor only on receipt-consistent rounds, from the post-review done set
- [task-state-is-the-folder](task-state-is-the-folder.md) — a task's state IS its directory; a bare `mv` to a missing state dir silently corrupts the queue
- [task-tmp-lifetime](task-tmp-lifetime.md) — task-local tmp survives resumable states but is containment-cleaned on done before review; artifacts persist
