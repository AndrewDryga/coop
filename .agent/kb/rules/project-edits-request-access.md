---
name: project-edits-request-access
description: project configuration requests access; one host approval grants it, checked before execution and enforced throughout agent and service runs
scope: security
sources: [internal/cli/net_approve.go, internal/box/network_approval.go, internal/networkstate/approval_review.go, internal/networkstate/admission.go, internal/box/filtered_services.go, internal/box/composecheck.go]
check: none
updated: 2026-09-12
---

# Approve added access, not ordinary development

Project files request permissions; they never grant them. The approved public interface is one
host-side `coop approve`, replacing `coop net approve`, for network permissions, service startup
and access to existing outside data. Store grants outside everything exposed to agents/services.
Check the effective permissions before starting affected code or mounting protected data. Use the
same captured configuration for checking and execution; save only what the operator reviewed.

Enforce grants with actual mounts/firewalls throughout agent and service lifetimes. Configuration
edits and later approvals do not widen running boxes. New permissions require approval and a new
run. A request field or process/API carrier cannot become authority; see
[[network-authority-is-proven-not-passed]].

**Why:** the user asked how unapproved edits could be kept from taking effect, then approved one
high-level command and this enforcement boundary. This is not a mandate to approve every risky file.

**How to apply:** review actual startup dependencies, volume identity and read/write access, not
just editable labels. Keep ordinary source/script/image changes, approved writes and project-owned
storage automatic. Unused services stay stopped without triggering approval. Preserve prohibitions
and destructive-command confirmations; do not convert them to reusable permission grants.
Tests must prove refusal before execution/mounts, same-snapshot publication/startup, and real runtime
denial from agent-controlled services. A command rename or UI warning is not enforcement.

## Changelog

- 2026-09-12 — recorded approved design. Swept the six source files and searched CLI/box/scaffold
  and generated docs for the old command. Existing network review captures requests for host-side
  publication; the command is still net approve, dependency-only approval and actual volume identity
  gaps remain in the audit, and service enforcement needs implementation. Queued as
  2026-09-12-unify-project-access-approval-under-coop-approve (audit S2/S3/S4). No whole-contract
  regression exists yet; check remains none rather than claiming current enforcement.
