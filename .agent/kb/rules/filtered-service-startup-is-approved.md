---
name: filtered-service-startup-is-approved
description: filtered auto-start runs only reviewed services and dependencies, without widening network grants or touching unrelated services
scope: security
sources: [internal/box/filtered_services.go, internal/box/filtered.go, internal/box/services.go, internal/box/composecheck.go, internal/box/serviceports.go, internal/networkstate/approval_review.go]
check: none
updated: 2026-09-12
---

# Start only reviewed services and dependencies in filtered runs

The unified `coop approve` review must name every service automatic startup requires,
including transitive dependencies. Starting a dependency is not permission for the agent to
connect directly to it; keep startup approval and network grants separate.

Validate and execute one private Compose snapshot containing only the reviewed startup set,
bound to the run's captured approval. Preserve dependency ordering and health/completion checks.
An empty approved set starts nothing. Scope generated overrides to that set. Do not remove
unrelated containers, start unrelated additions, or demand renewed approval just because an
unrelated service was added. Added startup dependencies or network/data permissions need review,
never unattended approval. Ordinary code/script/image changes within existing permissions do not.

**Why:** the user approved a fix where `api` and its reviewed database start, but a later-added
`background-uploader` does not. Normal dependencies must still work, without separate service
approval commands or extra launch prompts. Manual host `coop up` keeps its purpose and safety
checks; protected-data grants belong to the same authority. See [[project-edits-request-access]].

**How to apply:** retain reviewed dependency definitions even when no direct network rule names
them. Freeze configuration before validating it, then use the same bytes for startup and its
overrides. Avoid a blanket `--no-deps` workaround or `--remove-orphans` on filtered startup.
Test extra services, dependency changes, source swaps, empty approval, and unaffected manual
startup. Configuration snapshots do not freeze bind-mounted contents; keep that distinction.

## Changelog

- 2026-09-12 — user approved replacing net approve with unified coop approve and permission-based
  review instead of approval for ordinary contents. Updated the rule and sibling authority guidance;
  current source still exposes net approve. S2/S3/S4 implementation now belongs to child task
  2026-09-12-unify-project-access-approval-under-coop-approve; runtime gaps remain unimplemented.
- 2026-09-11 — recorded approved S2 design. Swept the six sources above: filtered startup checks
  the source before the shared starter rereads it; that starter uses project-wide ports and
  `up --remove-orphans` with no service names; approval storage drops dependency-only digests;
  startup loads the latest approval rather than binding its service set to the captured run.
  Gaps are queued under S2 in 2026-09-11-fix-defects-found-by-the-full-coop-feature-audit.
  No implementation regression enforces the complete design yet, so check remains none.
