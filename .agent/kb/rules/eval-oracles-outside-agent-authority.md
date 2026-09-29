---
name: eval-oracles-outside-agent-authority
description: hidden eval material stays outside every candidate access path, including mounts and Git history
scope: security
sources: [internal/box/run.go, internal/box/authority_mounts.go, internal/box/network_exposure.go, internal/box/derived_image.go, internal/box/taskchannel.go, internal/mcp/mcp.go, internal/eval/fixtures.go, internal/eval/suite.go, internal/eval/stage.go, internal/eval/stage_copy.go, internal/cli/eval_trial.go]
check: none
updated: 2026-09-29
---

# Keep hidden evaluation material outside candidate authority

An evaluated model MUST NEVER receive hidden tests, graders, reference solutions, expected answers,
full mock-response fixtures or grading records. This covers leads, delegates, reviewers, fallbacks
and their tools. Read-only mounts are exposure too. Only instructions, intended input data, visible
development tests and deliberately returned scenario tool results are candidate inputs.

**Why:** the user required that models never get evals through the Coop box, commit history or other
access paths; otherwise an apparent pass can be answer retrieval instead of task performance.

**How to apply:** validate the final mounts and services, including ancestor/companion mounts, homes,
volumes, links and tool-accessible resources. Materialize an approved clean tree with independent Git
metadata; remove hidden content from reachable history, reflogs, dangling objects and alternates,
not just HEAD. Do not rewrite the developer's source history. Keep hidden data out of image/build
layers, caches and provider histories. Grade a separate sanitized copy outside candidate authority,
and do not feed hidden grading back into a running loop. Use canary denial tests. Restricted public
evals must also deny direct and provider-mediated answer retrieval; prompts and domain filters
alone are not proof. This is a preventive eval contract, not a claim about generic box guarantees.

## Changelog
- 2026-09-29 — swept the candidate Git-history and launch canary checks and the snapshot
  retention path. The history probe now requires an empty search result, the candidate launch is
  identified independently of its mount contents, and the writable grading copy is discarded.
  Restricted-retrieval/live boundaries still need review, so `check: none` remains honest.
- 2026-09-28 — swept native eval staging and grader mount sources after the new retained-input
  path exposed a missing grader-only allowance. The loader and staging now reject cross-case and
  physical input/verifier overlap, and the candidate's no-files path stays empty; the exact retained
  verifier is admitted only read-only to a credential-free grader.
- 2026-09-13 — user correction recorded; swept the five source surfaces above and existing eval
  design. No eval runner exists yet. Generic mount exposure, image preparation and MCP projection
  are reuse seams, not eval isolation proofs; expanded the native-evals backlog task with explicit
  Git/mount/image/tool canaries, restricted-retrieval qualification and no live mock fallback.
  No executable eval-isolation gate exists yet, so check remains none.
