---
name: session-evidence-export
description: one bounded versioned read exports a session's network posture, observation, receipt and bound task to a fleet controller, with every section stating its own availability and unknown never collapsing into zero
subsystem: worker
sources: [internal/workerproto/session_evidence.go, internal/sessionsvc/evidence.go, internal/sessionsvc/http.go, internal/workerconnector/executor.go, internal/workerconnector/capabilities.go, internal/workerconnector/event_streams.go, internal/tasks/dir.go, docs/session-api.md]
updated: 2026-09-11
---

`GET /v1/sessions/{id}/evidence` is the daemon's own account of one session for a control plane's
inspection page, fetched by the `get_session_evidence` worker command and forwarded verbatim. Five
things about it are not obvious from the code.

**Every section fails on its own.** The posture (`mode`, `fingerprint`) comes from the immutable
session row, so it is always known; the captured policy, the run inventory, the newest run's
inspection and the session aggregate are four separate registry reads, and each one that fails
publishes `unavailable` with its reason while the others still publish. Collapsing them into one
503 would tell an operator nothing — and `unavailable`, `no_run` and `not_filtered` lead to three
different investigations, so they are three different words (`internal/sessionsvc/evidence.go`,
`networkEvidenceFromReads`).

**The export withholds destinations itself, not because upstream did.** `Snapshot.Project` has
already withheld names by the time the evidence read sees them, so a test built on a projected
snapshot proves nothing about this boundary. The projection here is an independent allowlist: a
denial or connection name, peer, rule id or drafted candidate crosses only under
`destinations-included`, and a withheld one says `destination_withheld` rather than reporting no
name. `workerproto.SessionEvidence.Validate` refuses the mismatch on both ends, so a leak fails
closed at the connector too.

**Counters are strings and null means unmeasured.** The collector's `networkview.Count` is an
unsigned decimal string precisely so a value above 2^53 survives JavaScript; the export keeps that
encoding and validates it back to 64 bits. A `nil` counter stays `null`. A coverage word this
contract does not know becomes `unavailable` carrying the original word as its reason — never
`exact`.

**Lists are bounded and the drop is counted.** Denials, connections, alerts, sources, run
references, checklist items and task files all have export bounds, and every truncation has its
own `omitted_*` count (the receipt's own `OmittedReferences` is added to what the export dropped,
so the total is honest). A run that denied a thousand names costs one bounded object that still
says how much it left behind.

**The task section is a snapshot, not a history.** A worker keeps no task transition ledger, so
the export publishes the folder as it stands, through the same `checkpointTaskProjection` a
checkpoint uses — which is why `state_sha256` here and in a checkpoint of the same folder state
are the same digest, and a control plane can link the two without guessing. `tasks.ScanChecklist`
supplies the labels the checkpoint's `Subtasks` booleans stand for, by the same scan, in the same
order. A bound task whose folder has gone missing reports `unavailable` with its identity intact.
The agent-written `state.md` is bounded, and secret-scanned on the WHOLE note before truncation so
a token past the bound still withholds the head. See [[worker-connector]] and [[network-consumers]].

**The `network` event is a consumer compatibility break, not just an addition.** Once
`operatorActivityEvent` admits `network`, a worker forwards that event to whatever control plane it
is enrolled with. A controller whose session-event validator knows only the activity kinds rejects
the payload — and because the event travels inside a poll, it rejects the WHOLE POLL, so one
filtered run that hits its boundary stops that worker polling entirely. Responder needed the kind
added to its own allowlist in the same change (`lib/responder/coop_fleet/protocol.ex`). Any other
control plane on this protocol needs the same before a worker carrying this build is pointed at it.

## Changelog
- 2026-09-11 — created with the evidence read, its connector command, the `session-evidence`
  capability proof and the `network` session event's outbound export.
