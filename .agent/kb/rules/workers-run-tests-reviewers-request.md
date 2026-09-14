---
name: workers-run-tests-reviewers-request
description: loop workers execute verification once; reviewers inspect the handoff and request missing checks through rework
scope: loop
sources: [internal/loop/prompts.go, internal/loop/review_packet.go, internal/loop/loop.go, README.md]
check: go test ./internal/loop -run 'TestLoopPreflightAndReviewFolder|TestReviewPromptForbidsTestExecution|TestReviewPacketCarriesWorkerReportedVerification'
updated: 2026-09-15
---

# Let workers run tests and make reviewers request missing evidence

A loop worker owns focused checks and the final required gate. It leaves a compact worker-reported
handoff in task state with the cwd, exact command and result, whether it covered final edits,
anything skipped, and an optional durable log. Review stages inspect the change, tests, and that
handoff without executing tests, gates, or services. Missing, failed, or stale verification is a
concrete finding that reopens the task and tells the worker exactly what to run.

**Why:** the user observed workers and multiple reviewers repeating the same broad commands and
chose a teammate model: "prompt the base model to run tests in the end and the review model to never
run them and trust they are okay." This keeps one owner for execution without treating reported
results as host attestation.

**How to apply:** Put the no-execution rule in the fixed footer shared by configured and built-in
review prompts. Carry only the bounded task-state summary by default; existing logs remain
inspectable. Do not add a test cache, command runner, output parser, or attestation layer. A review
that needs new runtime evidence reopens the task through the existing findings flow.

## Changelog

- 2026-09-15 — created after sweeping all three loop review stages, the shared review footer,
  worker prompt, compact packet, public loop docs, and Frontier role prompts. Removed the one
  automatic review-gate implementation; no other loop reviewer execution path remains.
