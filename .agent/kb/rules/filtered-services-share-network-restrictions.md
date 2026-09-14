---
name: filtered-services-share-network-restrictions
description: agent-controlled services must obey approved network restrictions without freezing live code or approving every image update
scope: security
sources: [internal/box/filtered.go, internal/box/filtered_services.go, internal/box/filtered_launch.go, internal/box/repo.go, internal/networkgateway/controller.go, internal/networkgateway/events.go, internal/networkgateway/guard.go, internal/networkview/records.go, internal/networkreport/report.go]
check: none
updated: 2026-09-14
---

# Enforce network restrictions for agent-controlled services too

Service-executed agent code must not provide an unrestricted network path around a filtered run.
Enforce approved access at runtime before service startup and across restarts/failures. Preserve
required service connections, live source/script mounts and normal image updates. Do not replace
network isolation with approval of every edited script or immutable image identity.

**Why:** the user explicitly approved "Apply the network restrictions to agent-controlled services
too" after withdrawing the audit's content-freezing prescription.

**How to apply:** verify allowed and denied traffic from an actually edited service script, including
startup/restart and enforcement failure. A shared bridge or proxy variable on an internet-capable
network is not enforcement. An internal network plus a policy-checking proxy is: removing the proxy
setting must leave the service offline, not unrestricted. Do not assume every service can work
offline; application APIs, workers, auth, storage and webhook integrations commonly need approved
outbound TLS.
Do not treat an existing unrestricted service as filtered or silently affect unrelated services.
Carry the exact prepared Compose service name through allowed and denied external traffic in Coop's
existing event, human-view and JSON paths. Internal service-to-service traffic stays direct and is
not external network evidence. Observation must not grant access or create a second tracing system.
Use the separately approved unified permission flow in [[project-edits-request-access]]; it does
not expose new credentials or grant extra direct agent-to-service connections.

## Changelog

- 2026-09-15 — filtered services now use the owning run's private Compose project and network;
  the same run identity continues to attribute their external traffic.

- 2026-09-14 — added source attribution to existing network views after the user asked to observe
  and approve service traffic through the same CLI UX. The exact prepared service/IP binding is the
  identity; internal peer traffic is not recorded as external traffic.

- 2026-09-13 — corrected the internal-only assumption after the user named real application-service
  outbound needs. Selected service closures now use an internal network for direct peer traffic and
  the existing guard for approved TLS; direct internet remains unavailable without the proxy route.

- 2026-09-12 — subsequent approval places S2/S3/S4 under unified coop approve; removed the stale
  command-decision caveat. Implementation is in 2026-09-12-unify-project-access-approval-under-coop-approve.
- 2026-09-12 — recorded S4 approval. Swept the three sources above: services start independently,
  the controller attaches to their network, only the agent shares its namespace, and current
  kernel rules filter local output while dropping forwarding. Service-egress enforcement remains
  queued as S4 in 2026-09-11-fix-defects-found-by-the-full-coop-feature-audit. No runtime regression
  covers the approved behavior yet; check is none.
