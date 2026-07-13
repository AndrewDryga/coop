# .agent/kb — the committed knowledge base

Descriptive operational knowledge an agent needs but the code doesn't obviously carry: subsystem
maps, cross-cutting traps, hard-won gotchas. Sibling of `rules/` — but `rules/` is NORMATIVE
("do X, not Y") while a card here is DESCRIPTIVE ("here's how X actually works, and the trap"). A
rule may link to a card for background.

## Reading protocol
Read this INDEX at boot; open a card ONLY when your task touches its subsystem. Never bulk-load the
kb into a prompt — the index is the routing table, the cards are pulled on demand (like skills).

## Card format
One fact per file: frontmatter plus a short body (keep it under a screen).

```
---
name: <kebab-case-slug>
description: <one line — used to judge relevance straight from the index>
verified: <YYYY-MM-DD — when this was last checked against the source>
retire-when: <the condition that makes this card wrong or obsolete>
---
<the fact; cite file:line for load-bearing claims; link related cards with [[name]]>
```

## The inbox — how the kb grows without rotting
An unattended agent must NOT edit the live kb — that's a self-modifying prompt. Instead it drops a
DRAFT card into `inbox/`. Inbox cards are committed but NEVER loaded into a prompt. A human (or an
explicitly-invoked review) promotes one by moving it up into `kb/`, stamping `verified:`, and adding
its line to the index below — a git-reviewed folder move, the same shape as the task backlog. A card
that has drifted from the source is worse than none: re-check `verified:` when you touch its
subsystem, and delete a card whose `retire-when` has come true.

## Index
- [box-time-is-utc](box-time-is-utc.md) — boxes run UTC; the host TZ is forwarded so rate-limit reset prose parses back host-local
- [credentials-expired-is-a-false-alarm](credentials-expired-is-a-false-alarm.md) — claude "token expired" still works in-box via the refresh token
- [task-state-is-the-folder](task-state-is-the-folder.md) — a task's state IS its directory; a bare `mv` to a missing state dir silently corrupts the queue
