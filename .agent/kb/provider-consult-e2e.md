---
name: provider-consult-e2e
description: Verify generated coop-consult behavior through all provider arms, fallback pairs, and a four-edge live ring
subsystem: testing
sources: [Makefile, internal/consult/wrapper.go, internal/consult/instructions.go, internal/preset/contract.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/agent/consult_shell.go, internal/agent/role_health.go, internal/preset/wrapper.go, internal/loop/telemetry.go, internal/loop/streamjson_providers.go, internal/cli/scripted_consult_process_e2e_test.go, internal/cli/provider_consult_live_e2e_test.go, internal/cli/testdata/providerfixture/main.go, internal/testutil/liveprovider/contract.go, internal/testutil/liveprovider/cleanup.go]
updated: 2026-09-19
---

`make provider-scripted-e2e` is the blocking consult contract. A strict external Coop binary mounts
the production-generated wrapper into the fixture box, which validates its exact bytes and invokes
only fixed semantic provider aliases. The registry-derived suite covers all four fresh/resume arms
and all 12 ordered distinct fallback pairs. It owns rate limits, failed resume, missing ids,
timeouts, empty/malformed/stderr-only/ordinary failure, bounds, scope denial, Codex telemetry,
repository integrity, and process cleanup without real credentials or quota.

Continuation state is one versioned record replaced atomically under the box `TMPDIR`. Candidate
native ids are published only after a usable stdout reply. Stderr is diagnostic only. A
failed resume
clears its uncertain id but retains the last complete transcript; the next `--continue` starts that
same successful rung fresh. Only a proven nonzero rate limit advances a ladder. Reply and diagnostic
streams are unlimited by default; an explicit COOP_CONSULT_STREAM_LIMIT caps each independently.
Prompt and transcript state cap independently at 512 KiB.

All built-in consult arms capture native JSON and deliver only decoded answer text. Shared
capture and guarded best-effort append live in `internal/agent/consult_shell.go`; adapters own
the JSON filters. A row is published only after the complete attempt (reply and diagnostics)
is accepted. Claude and Grok may report invocation-local dollars; Gemini and Codex report
tokens without invented prices. Claude input adds cache writes/reads, Gemini input already
includes cached input, and current Grok output includes reasoning. Missing or invalid usage
never invalidates an otherwise usable reply. Delegate diff-report output is unchanged and
delegate calls use the same adapter-owned parser to append one usage row after a successful
attempt. Failed or malformed attempts keep usage unknown, and a nested consult retains its own
separate row rather than being folded into the delegate total.
All-provider parser and generated-wrapper unit tests cover the added usage/cost paths;
the scripted process suite's explicit telemetry scenario currently covers Codex only.

Native fresh/resume feasibility captures on 2026-09-13 qualified Claude 2.1.267 JSON,
Gemini 0.59.0 stream-json, and Grok 1.0.25 streaming-json session flags and field shapes.
These narrow captures do not replace the final installed wrapper-in-loop qualification.
Grok lead and consult decoding also retain older separate-reasoning streams: a positive native
total equal to input plus output identifies inclusive output; otherwise its legacy
reasoning addition remains. Optional malformed cost is ignored without losing a terminal
event or valid token usage. No version guessed from the selected model name.
Same-target calls serialize on a private lock. The Coop image uses `flock`, so the kernel releases
ownership after an unclean exit. A custom image without `flock` uses a fail-closed `mkdir` fallback;
after confirming no consult is active, remove its private `.lock.d` directory.

Preset role health shares the host-created private `<run>.peers.jsonl` ledger with usage rows.
Wrappers resolve the Git top level when they append, so a consult launched from a monorepo
subproject still reaches the run ledger. Quarantine is exact-target and run-scoped, and only a
recorded permanent failure activates it. The empty ledger must therefore decode to an explicit
false: jq 1.6 can exit zero for `jq -e select(...)` over an empty stream, which otherwise makes
every advisor look already quarantined before its first call. `coop_role_quarantined` slurps the
bounded ledger and tests the explicit boolean returned by `any(...)` instead.

Permanence differs by wrapper. The consult's `coop_failure_permanent` reads its stderr for broken
invocations and refused logins; the delegate records only exit 126/127 and `coop_login_rejected` on
the provider's own stderr (`provider-stderr-<n>`), because its merged output carries the agent's reply
and tool noise. Both hand a permanent failure to the next rung. The ledger sits in the worktree, so
the delegate's "nothing changed" snapshot excludes `.agent/runs`: its own failure row, appended before
the comparison, otherwise stopped every in-run fallback as "changed ignored files".

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

Triage `skipped` as a prerequisite first: `credential_refresh_required` needs trusted-source
renewal before an access-only projection, not necessarily re-login (see
[[credentials-expired-is-a-false-alarm]]);
`credential_not_portable` needs an env-backed key; `ring_prerequisite` means another edge was not
ready and no paid call ran. `failed` after `attempted=true` is upstream compatibility or provider
behavior. `repository_changed`, `source_changed`, `cleanup_failed`, and `harness_failed` are local
isolation failures and take precedence. Raw output is intentionally absent; reproduce adapter
syntax and faults in the deterministic fixture instead of retaining a live response.

Successful transport is not completed source review: a peer can return useful advice while
reporting denied source reads or unrun checks. Normal and preset lead guidance requires full
reply/material-diagnostic reading in bounded chunks and preserving unresolved qualifications
in final/state/log. Wrapper fixtures preserve a partial reply at exit0; they do not prove a
native lead carries its caveats through synthesis. No prose-to-verdict parser is involved.

## Changelog
- 2026-09-19 — the delegate hands a permanent failure (126/127, a refused login on stderr) to its next
  rung and quarantines it; its tree snapshot now excludes the run ledger, which had stopped every
  in-loop fallback.
- 2026-09-15 — reused the four adapter-owned structured-output parsers for delegate usage;
  success, failure, diagnostics, and nested-consult tests preserve the existing reply/report
  contract without adding pricing guesses or a separate telemetry layer.
- 2026-09-14 — documented the shared role-health ledger, repository-root resolution, and the
  jq 1.6 empty-stream trap found during a real Frontier run.
- 2026-09-13 — Grok native captures also reproduced a lead-decoder accounting bug:
  preserved older separate-reasoning streams while honoring current inclusive totals
  and provider-reported cost; focused lead/consult fixtures cover both formats.
- 2026-09-13 — extended consult usage to all built-in adapters and optional reported cost;
  documented captured token semantics, best-effort publication and deferred native proof.
  Rechecked credential preflight advice: a refreshable token is not a lost login.
- 2026-09-13 — documented review-evidence carry-through and partial-versus-complete wrapper
  fixtures; rechecked configured output bounds and corrected stale fixed1MiB default prose.
- 2026-08-25 - source path moved from `internal/fusion` to `internal/consult`; wrapper contract unchanged
- 2026-07-15 - created with the complete deterministic matrix and isolated four-edge live ring
