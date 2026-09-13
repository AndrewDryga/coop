---
name: filtered-services-share-network-restrictions
description: agent-controlled services must obey approved network restrictions without freezing live code or approving every image update
scope: security
sources: [internal/box/filtered.go, internal/box/filtered_launch.go, internal/networkgateway/controller.go]
check: none
updated: 2026-09-12
---

# Enforce network restrictions for agent-controlled services too

Service-executed agent code must not provide an unrestricted network path around a filtered run.
Enforce approved access at runtime before service startup and across restarts/failures. Preserve
required service connections, live source/script mounts and normal image updates. Do not replace
network isolation with approval of every edited script or immutable image identity.

**Why:** the user explicitly approved "Apply the network restrictions to agent-controlled services
too" after withdrawing the audit's content-freezing prescription.

**How to apply:** verify allowed and denied traffic from an actually edited service script, including
startup/restart and enforcement failure. A shared bridge or proxy variable is not enforcement.
Do not treat an existing unrestricted service as filtered or silently affect unrelated services.
Use the separately approved unified permission flow in [[project-edits-request-access]]; it does
not expose new credentials or grant extra direct agent-to-service connections.

## Changelog

- 2026-09-12 — subsequent approval places S2/S3/S4 under unified coop approve; removed the stale
  command-decision caveat. Implementation is in 2026-09-12-unify-project-access-approval-under-coop-approve.
- 2026-09-12 — recorded S4 approval. Swept the three sources above: services start independently,
  the controller attaches to their network, only the agent shares its namespace, and current
  kernel rules filter local output while dropping forwarding. Service-egress enforcement remains
  queued as S4 in 2026-09-11-fix-defects-found-by-the-full-coop-feature-audit. No runtime regression
  covers the approved behavior yet; check is none.
