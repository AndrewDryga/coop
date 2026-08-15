---
name: session-api-dto-is-a-second-projection
description: A field on session.Turn/Session is invisible to API clients until the hand-written DTO and its public* copier carry it too.
subsystem: sessions
sources: [internal/sessionsvc/http.go, internal/session/records.go, internal/sessionsvc/http_test.go, internal/sessionsvc/activity.go]
updated: 2026-08-15
---

The durable record and the public wire type are two separate hand-maintained structs. `session.Turn`
(`internal/session/records.go`) is what the store reads and writes; `TurnDTO`
(`internal/sessionsvc/http.go`) is what `/v1/sessions/...` returns, and `publicTurn` copies field by
field. Nothing — not the compiler, not a test — ties the two together, and the DTOs deliberately do
not mirror the records, because the records carry prompts, host paths and native session IDs that
must never reach a client. So the gap is a feature, and that is exactly why a new field falls into
it silently: the store, the schema, the scanner and the ingest path can all be correct and complete
while the API returns nothing.

Per-turn token usage shipped that way. `99dcdd5` added `Turn.Usage`, four schema v5 columns, the
scan and the ACP parse; `3c3d65a` fixed the parse half an hour later so real numbers arrived; the
extraction into `internal/sessionsvc` moved the file wholesale. Not one of the three touched the
DTO, so `GET /v1/sessions/{id}/turns/{id}` answered with no `usage` key at all while the database
held the measurements — the feature looked entirely wired from inside and published nothing.

When you add a field a client is supposed to see, the change is four places, not two: the record,
the store, the DTO, and a test in `internal/sessionsvc/http_test.go` that reads the field back off
the wire — decoded from the JSON, not through `TurnDTO`, so it fails on the bytes a caller receives
rather than compiling against the type you just fixed.

The same seam runs through `publicSession`, `publicOperation` and `publicEvent`. `OperationDTO`
drops `Operation.Result` on purpose and must keep doing so; do not "fix" that by copying the field
across. `EventDTO` used to drop the payload for the same reason, and that turned out to be the
wrong call: a caller could count that a turn failed without being told why, watch a turn run
without seeing any of the work inside it, and — until `provider.backoff` — could not distinguish a
turn crawling through provider 429s from a dead one. It now carries `Event.Payload`, so a new event
type is only useful once its payload says something, and `internal/sessionsvc/http_test.go` is
where that is proven off the wire.

Two events answer that last question, and they are not interchangeable. `provider.backoff` is a
decision Coop's own target ladder made, so it exists only for a limit that reached the ladder as an
ACP rejection. A provider CLI that retries its 429s INTERNALLY never produces one: it streams frames,
makes no tool calls, and its turn is silent end to end — which is most of what a real throttle storm
looks like. `provider.alive` (`internal/sessionsvc/activity.go`) covers that half from the frame pump
instead, bounded to one per minute and suppressed by any narration in the same window. When you
reach for "is this turn alive", ask which half you are in first.

See also [[acp-generated-output-boundary]] (what public turn JSON may contain, and what stays behind
the artifact endpoint).

## Changelog
- 2026-08-15 — recorded the split between `provider.backoff` (a ladder decision) and `provider.alive`
  (the frame pump's own pulse), because the card's "crawling through 429s" line read as if the first
  had covered the whole problem; it only covered the limits the ladder can see. `sources` gains
  `internal/sessionsvc/activity.go`, where the pulse and its window live.
- 2026-08-09 — created after `TurnDTO` was found dropping `Usage`; verified against `publicTurn`, `publicSession`, `publicOperation` and `publicEvent` in `internal/sessionsvc/http.go`.
- 2026-08-15 — drift: the card still said `EventDTO` never exposes a payload, which stopped being true when turn narration shipped; re-read `publicEvent` and `EventDTO` and corrected it. `docs/session-api.md` carried the same stale claim and was fixed in the same commit.
