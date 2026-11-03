---
name: case-exposure-has-persistent-approval
description: require explicit coop approve --case-exposure for mount-fallback file access without granting future paths or policy writes
scope: security
sources: [internal/box/caseapproval.go, internal/box/mounts.go, internal/cli/net_approve.go, internal/cli/services_cmd.go, internal/runtime/docker_objects.go, internal/runtime/docker_lifecycle.go]
check: "go test ./internal/cli -run 'TestApproveDoesNotRequestCaseExposure|TestApproveExplicitCaseExposure|TestApproveDoesNotInspectMountPolicy|TestMountFallbackRemedyRequiresExplicitCaseApproval'"
updated: 2026-11-03
---

# Remember reviewed case exposure through coop approve

A user who accepts secret-file exposure on a case-insensitive disk can save that decision
with `coop approve --case-exposure`. Ordinary `coop approve` must neither compute mount
exposure nor create or rewrite file-access approval. Qualified eBPF runs need no such permission;
the explicit review describes mount fallback only. Do not predict all future launch backends
from host capability: images and runtime options can also force mount protection. Actual mount
refusals must direct the explicit command, so accepting fallback remains possible.
Bind approval to the host-owned repository identity and exact reviewed paths;
new paths require another review. Keep canonical decoys. Do not grant alternate-spelling
writes to `.coopignore` through this secret-file approval. Show paths during review and a
short count during launch. Check both agent and service mount plans.

Docker creation and observation must support the mount plans case protection produces.
Bounds on inspection must not strand a successfully created ordinary box.

Coop must work with existing supported host filesystem configurations and the user's shared
checkout. Do not require users to move projects onto a case-sensitive host volume, reformat
storage, or change filesystem settings as the default solution. Handle runtime/share differences
inside Coop and distinguish usability from the strength of secret isolation. A diagnostic warning
is not enforcement; preserving strong isolation on an alias-exposing share requires a mechanism
that actually denies access through those aliases. Qualify any replacement on the real runtime
before presenting it as a supported solution.

**Why:** the user reported Coop was unusable: repeating
`COOP_ALLOW_CASE_EXPOSED_SECRETS=1` was annoying, and the resulting launch failed inspection.
They explicitly requested removal of the feature or persistent approval via `coop approve`.

## Changelog
- 2026-11-03 — user reported Emisar's approval prompt despite eBPF. Swept approval dispatch,
  agent/service refusal remedies, help/completion, box/configuration/networking docs and KB.
  Made mount exposure an explicit option; retained existing grants and runtime enforcement.
  Regression tests first reproduced unsolicited file approval during both unchanged and changed
  network reviews, and now cover default no-grant behavior and the explicit fallback remedy.
- 2026-11-01 — user corrected the host-volume recommendation: "we need coop to work no matter
  how host is configured". Swept box/configuration docs, spelling detection, case approval and
  doctor source: no existing requirement to move projects to a case-sensitive volume. Recorded
  the design constraint; this prose update does not implement or qualify a replacement mount
  mechanism. The automated check above covers approval; host independence also needs review.
- 2026-11-01 — swept agent launch, service launch, approval UI, spelling budgets and Docker
  create/inspect limits. Added exact-path approval, case-only review, both launch checks, and
  aligned Docker bounds. The failed live container had 1,780 mounts and 772,918 bytes of mount
  JSON against a 256 KiB inspection limit. Tests cover persistence, new-path refusal,
  copied/replaced identity, cancellation, and successful 2,200-mount creation and inspection.
