---
name: controller-can-read-job-output
description: a job's controller can read its complete check output automatically; response size limits never make a tail the only evidence
scope: architecture
sources: [internal/forkctl/merge.go, internal/sessionsvc/review.go, internal/box/run.go]
check: none
updated: 2026-09-28
---

# Give the job's controller access to complete check output

Ryker must be able to read its job's complete stdout and stderr, including errors, warnings,
normal output and successful checks, without a separate output-export opt-in or the user
relaying worker logs. A short summary or tail may be convenient, but cannot be the only output
available to the authorized controller.

Bound individual responses and memory with incremental reads or streaming backed by job-owned
logs. Do not silently discard earlier diagnostics. Report capture, retention or resource failures
explicitly. Keep authentication and job ownership checks; fix cross-job mounts and credential
isolation at the sandbox boundary rather than hiding ordinary job output.

**Why:** On 2026-09-28 the user rejected both an opt-in and a tail-only design: "Ryker should get
full access to errors, warnings and all other output to work, like any llm model would, it's a
sandbox!!"

**How to apply:** Test full retrieval larger than one response, both output streams, warnings on
success, failure/cancellation and cross-job access denial. A model need not receive the whole log
in one context window; it must be able to retrieve the rest. See
[[transport-bounds-do-not-abort-valid-work]] and [[static-bounded-supervision]].

## Changelog

- 2026-09-28 — created from the output-access correction. Swept the three source files and
  active queue proposals: one existing review-output gap (worker stdio plus a status-only
  controller result), owned by task
  `2026-09-28-decide-what-a-failed-review-gate-reports-to-the`. Its proposed opt-in/64 KiB tail is
  superseded. Local merge gates still inherit terminal output; no separate sibling correction
  identified. Enforcement tests belong to that implementation; this rule is currently review-only.
