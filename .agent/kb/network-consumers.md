---
name: network-consumers
description: how the loop, direct/ACP runs and remote sessions consume one frozen network capture, and which surface reads which evidence
subsystem: networking
sources: [internal/loop/network.go, internal/loop/host.go, internal/cli/commands.go, internal/cli/net_cmd.go, internal/cli/net_diagnostic.go, internal/box/network_session.go, internal/sessionsvc/network.go, internal/sessionsvc/acp.go, internal/sessionsvc/http.go, internal/session/schema.go, internal/workerproto/protocol.go]
updated: 2026-09-10
---

Admission happens ONCE per unit of work and the resulting `*box.CapturedEgress` is passed down; no
consumer admits twice. See [[restricted-networking]] for what admission decides.

**Direct runs** admit in the CLI and hand the capture to `box.Run` on the same host process
(`cli/commands.go:203`). A fork loop re-renders the operator's flags in their exact spelling so the
detached worker admits what its foreground twin would have (`cli/network_flags.go:84`).

**The loop** admits once, right after the review ladders are built, and every box it launches —
pre-flight, work, review, verify, debug shell — attaches that same capture through one `runBox`
seam (`loop/host.go:116`). The admission spec unions every rung of every stage's ladder plus the
named peers (`loop/network.go:29`): a provider that rotates in mid-drain must already be in the
frozen policy, or it would meet a denial instead of a refusal at launch. Refusals accumulate during
an iteration and print between iterations, never over the live bar, plus one ranked closing summary
(`loop/network.go:91`, `:158`).

**Remote sessions** freeze the posture at create. The session row stores mode, owner-keyed
fingerprint and qualification id (schema 21, `session/schema.go:245`); every later run — cold turn,
warm child, resume, replay — loads that snapshot instead of admitting again, so an approval or
config edit landing mid-session can only produce a visible denial. Then:

- The daemon is the child's HOST parent and scrubs every `COOP_*` from the environment it builds, so
  `COOP_NETWORK_CAPTURE` (`box/network_session.go:22`) can only be set there
  (`sessionsvc/network.go:129`). It carries a *reference* — project, fingerprint, qualification,
  session, attempt — not authority.
- The child proves it: `OpenExisting` (never `Open`, so a child cannot create an owner key) plus
  `LoadSnapshot` of that exact project+fingerprint pair (`box/network_session.go:195`). A forged or
  cross-project reference produces no snapshot and no launch.
- After the child exits the daemon checks every run registered against the session against the
  immutable row's fingerprint and FAILS the turn on a mismatch (`sessionsvc/network.go:210`).
- A filtered child owns a gateway, its volumes and its receipt, so it gets a 30 s stop window
  instead of the ordinary 250 ms (`sessionsvc/acp.go:48`). Killing it fast leaves two running
  containers and a receipt nothing can finalize.

The API adds `GET /v1/sessions/{id}/network` (live) and `/network/receipt` (aggregate, final once
closed) (`sessionsvc/http.go:483`), a `network` event on the session stream whose payload is bounded
by construction (refusals grouped and capped, alerts capped), `SessionDTO.network`, and the worker
commands `get_network` / `get_network_receipt` (`workerproto/protocol.go:29`). None of them is a
path to authority: the API has no approval verb at all.

`coop net` is the host operator's read surface, and its verbs differ in what they read
(`cli/net_cmd.go:49`):

| verb | reads |
| --- | --- |
| bare `coop net` | this project's remembered posture plus its pending request; creates no owner key |
| `setup` | the only mutating verb besides `approve`: builds the image pair and records the host qualification |
| `approve` | TTY only; the repo's request envelope diffed against the remembered approval, digest-fenced |
| `ls` · `inspect` · `watch` · `receipt` | the keyless `Evidence` handle over retained execution records — no runtime, no DNS, no key |
| `why` | the run's captured policy, evaluated hypothetically; it never sends a packet |
| `explain` | one RETAINED denial event plus the draft rule a human could add; a DNS-only refusal drafts nothing, because it cannot prove TLS/443 |

`receipt` is redacted by default because it is the export artifact;
`inspect`/`why`/`explain`/`watch` show names, as the local operator view. An event that has aged
out of a run's bounded ring reports `event_not_retained` — which is not proof the id ever existed.

## Changelog
- 2026-09-10 — created from the shipped consumers (S3–S6, 4739bb1…f3e1d96), replacing the WIP
  session/ACP/diagnostics cards. Verified against the sources above.
