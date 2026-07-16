---
name: provider-consult-e2e
description: Verify generated coop-consult behavior through all provider arms, fallback pairs, and a four-edge live ring
subsystem: testing
sources: [Makefile, internal/fusion/wrapper.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/cli/scripted_consult_process_e2e_test.go, internal/cli/provider_consult_live_e2e_test.go, internal/cli/testdata/providerfixture/main.go, internal/testutil/liveprovider/contract.go, internal/testutil/liveprovider/cleanup.go]
updated: 2026-07-15
---

`make provider-scripted-e2e` is the blocking consult contract. A strict external Coop binary mounts
the production-generated wrapper into the fixture box, which validates its exact bytes and invokes
only fixed semantic provider aliases. The registry-derived suite covers all four fresh/resume arms
and all 12 ordered distinct fallback pairs. It owns rate limits, failed resume, missing ids,
timeouts, empty/malformed/stderr-only/ordinary failure, bounds, scope denial, Codex telemetry,
repository integrity, and process cleanup without real credentials or quota.

Continuation state is one versioned record replaced atomically under the box `TMPDIR`. Candidate
native ids are published only after a bounded usable stdout reply. Stderr is diagnostic only. A
failed resume
clears its uncertain id but retains the last complete transcript; the next `--continue` starts that
same successful rung fresh. Only a proven nonzero rate limit advances a ladder. Reply and diagnostic
streams cap independently at 1 MiB; prompt and transcript state cap independently at 512 KiB.
Same-target calls serialize on a private lock. The Coop image uses `flock`, so the kernel releases
ownership after an unclean exit. A custom image without `flock` uses a fail-closed `mkdir` fallback;
after confirming no consult is active, remove its private `.lock.d` directory.

`make provider-consult-live-e2e COOP_LIVE_TARGETS='claude,codex,gemini,grok'` is the permissive
upstream ring; `make provider-consult-live-e2e-all` is strict. The clean child directly invokes each
mounted peer role once, so a complete ring starts four peer CLI sessions and zero lead sessions.
Provider tool round-trips may add upstream turns. Each registry-derived edge proves real lead-plus-peer
credential scope and wrapper wiring; the deterministic 12-pair matrix owns every ordering and fallback policy.
All credentials and version probes must pass before admission. Every edge mounts exactly its lead and peer credentials and gets
one hard-deadline session in a writable disposable repo, no retry, a unique marker, and explicit
tracked/untracked/ignored write attempts. Source credentials, complete repository bytes/modes, Git semantic axes,
revocation, CIDs, labels, and per-edge process quiescence are verified. Evidence is one redacted
`COOP_CONSULT_LIVE_SUMMARY` line.

Triage `skipped` as a prerequisite first: `credential_refresh_required` needs re-login;
`credential_not_portable` needs an env-backed key; `ring_prerequisite` means another edge was not
ready and no paid call ran. `failed` after `attempted=true` is upstream compatibility or provider
behavior. `repository_changed`, `source_changed`, `cleanup_failed`, and `harness_failed` are local
isolation failures and take precedence. Raw output is intentionally absent; reproduce adapter
syntax and faults in the deterministic fixture instead of retaining a live response.

## Changelog
- 2026-07-15 - created with the complete deterministic matrix and isolated four-edge live ring
