---
name: session-api-dto-is-a-second-projection
description: A field on session.Turn/Session is invisible to API clients until the hand-written DTO and its public* copier carry it too.
subsystem: sessions
sources: [internal/sessionsvc/http.go, internal/session/records.go, internal/sessionsvc/http_test.go]
updated: 2026-08-09
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

The same seam runs through `publicSession`, `publicOperation` and `publicEvent`. Two of those drop
data on purpose and must keep doing so: `OperationDTO` never exposes `Operation.Result`, and
`EventDTO` never exposes an event payload — a client is expected to re-fetch the turn or the session
the event names. Do not "fix" those by copying the field across.

See also [[acp-generated-output-boundary]] (what public turn JSON may contain, and what stays behind
the artifact endpoint).

## Changelog
- 2026-08-09 — created after `TurnDTO` was found dropping `Usage`; verified against `publicTurn`, `publicSession`, `publicOperation` and `publicEvent` in `internal/sessionsvc/http.go`.
