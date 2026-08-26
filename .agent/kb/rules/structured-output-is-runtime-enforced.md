---
name: structured-output-is-runtime-enforced
description: "a structured-output schema is durable execution control, validated before completion, never prompt-only advice"
scope: architecture
sources: [internal/session/output_contract.go, internal/session/store.go, internal/sessionsvc/acp.go, internal/sessionsvc/http.go, docs/session-api.md]
check: "go test ./internal/sessionsvc -run 'TestInvalidStructuredResultIsRepairedBeforeTheTurnCompletes|TestRepeatedInvalidStructuredResultNeverCompletes|TestSchemaValidSemanticResultWaitsForCallerAcceptance|TestRejectedSemanticResultRepromptsTheSameNativeTurn|TestSchemaRepairDoesNotSpendASemanticCandidateAttempt'"
updated: 2026-08-26
---

# Enforce structured output at the runtime completion boundary

Persist the caller's exact JSON Schema on the turn, show it to the model, validate the final bytes
inside Coop, and retry a rejected candidate in the same native session. When the caller owns
semantic rules over frozen external state, keep a schema-valid result unpublished until the caller
accepts its exact digest. Never mark an invalid or unaccepted candidate complete or rely on a prompt
saying that the model should validate itself.

**Why:** Responder gave models its result schema and asked them to self-check, but malformed JSON
still completed in Coop and consumed Responder correction rounds. The operator's correction was:
"Model should ALWAYS validate output" and "Coop can validate and retry too, not letting model to
finish its turn unless it's valid."

**How to apply:** A structured caller submits a bounded `output_contract` with the exact schema
bytes and SHA-256 digest. Admission compiles it, the turn persists it, and the ACP runner checks it
before `CompleteTurn`. With `require_semantic_validation`, Coop stages an `awaiting_validation`
candidate, exposes its digest, and accepts or rejects only that digest through an idempotent
operation. Rejections are visible as events and resume the same logical/native turn. Semantic
review permits three schema-valid candidates; each semantic round has its own three-response schema
repair bound, so malformed JSON never spends a caller-review attempt. A restart preserves the
candidate; acceptance returns a durable receipt.

## Changelog
- 2026-08-26 — added the public API guide to the rule's sources and corrected its stale 128/64 KiB
  limits plus the omitted contract shape, schema bound, digest, turn-body exception, and validation
  endpoint. Also gated the separate schema/semantic counters after review caught prose collapsing
  them into one budget. Runtime behavior was already enforced; the published caller contract had
  drifted.
- 2026-08-25 — extended the completion boundary to caller-owned semantic validation; swept the
  session store, ACP runner, HTTP DTO/route, restart state and cancellation path. Focused tests cover
  accept, stale digest, rejection/resume, exhaustion, HTTP delivery and same-native-session repair.
- 2026-08-21 — created after sweeping the session admission, persistence, HTTP, and ACP completion
  path. The one violation was prompt-only schema guidance; the durable contract and repair loop fix
  it. The focused tests failed against the old behavior before the fix and now cover persistence,
  repair, and bounded exhaustion.
