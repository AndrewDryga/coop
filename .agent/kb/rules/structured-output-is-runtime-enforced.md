---
name: structured-output-is-runtime-enforced
description: "a structured-output schema is durable execution control, validated before completion, never prompt-only advice"
scope: architecture
sources: [internal/session/output_contract.go, internal/session/store.go, internal/sessionsvc/acp.go, internal/sessionsvc/http.go]
check: "go test ./internal/sessionsvc -run 'TestInvalidStructuredResultIsRepairedBeforeTheTurnCompletes|TestRepeatedInvalidStructuredResultNeverCompletes'"
updated: 2026-08-21
---

# Enforce structured output at the runtime completion boundary

Persist the caller's exact JSON Schema on the turn, show it to the model, validate the final bytes
inside Coop, and retry a rejected candidate in the same native session. Never mark an invalid
candidate complete or rely on a prompt saying that the model should validate itself.

**Why:** Responder gave models its result schema and asked them to self-check, but malformed JSON
still completed in Coop and consumed Responder correction rounds. The operator's correction was:
"Model should ALWAYS validate output" and "Coop can validate and retry too, not letting model to
finish its turn unless it's valid."

**How to apply:** A structured caller submits a bounded `output_contract` with the exact schema
bytes and SHA-256 digest. Admission compiles it, the turn persists it, and the ACP runner checks it
before `CompleteTurn`. Rejections are visible as events and repair attempts stay bounded. Semantic
checks that need caller-owned evidence remain in the caller after Coop's structural gate.

## Changelog
- 2026-08-21 — created after sweeping the session admission, persistence, HTTP, and ACP completion
  path. The one violation was prompt-only schema guidance; the durable contract and repair loop fix
  it. The focused tests failed against the old behavior before the fix and now cover persistence,
  repair, and bounded exhaustion.
