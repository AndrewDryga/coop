---
name: coopignore-read-only-in-boxes
description: keep .coopignore readable but read-only in boxes, preserve ordinary host editing, and avoid a separate policy-approval workflow
scope: security
sources: [internal/box/mounts.go, internal/box/secrets.go, internal/box/run.go, internal/box/serviceshadow.go, internal/box/image.go, internal/box/mounts_test.go, internal/shadowpath/shadowpath.go]
check: none
updated: 2026-10-18
---

# Mount .coopignore read-only while preserving normal host editing

Root and nested `.coopignore` files should remain readable in boxes and service containers, but
those containers must not be able to edit, truncate, delete or replace them through any mounted
path. Account for alternate service targets and symlink/hardlink aliases. The user edits the
repository file from the host as usual; new boxes use the changes without an approval workflow.

Do not hide the policy behind an empty decoy, change host file permissions, or require a separate
approved-policy copy or confirmation to edit it. Existing unreadable or invalid policy must fail
safely rather than silently become an empty rule set. A missing policy is not a reason to create
an empty file in the user's repository.

**Why:** after a proposal for a persistent host protection floor and approval prompts, the user
asked, "Should we just mount that file readonly or not mount at all?" and approved the read-only
recommendation. The accepted scope protects editing of the rules while keeping ordinary host
editing and shared configuration convenient.

**How to apply:** verify actual container write attempts, alternate mounts, link aliases and host
editor atomic saves. Keep parent-folder relocation as a separate containment repair: a child decoy
does not by itself freeze the parent path, and read-only policy mounts alone do not prove safety
on the next launch or build. Do not treat a mount-plan unit test as the complete runtime proof.

## Changelog
- 2026-10-18 — fork merge and the session review scan flag a change to `.coopignore` in any case
  (box.IsCoopignoreName), including a folder of that name (task
  2026-10-18-flag-a-fork-or-session-change-to-a-coopignore-sp).
- 2026-10-18 — a rules file under another spelling is read-only too: on a case-insensitive Mac
  disk the decider opens .coopignore and gets .COOPIGNORE, so the mount plan and both service-shadow
  walks now match by file identity (box.isCoopignore), not by spelling (task
  2026-10-17-mount-a-coopignore-spelled-in-another-case-read). Sweep: fork merge and the session
  review scan compare git paths to .coopignore exactly; queued as task
  2026-10-18-flag-a-fork-or-session-change-to-a-coopignore-sp.
- 2026-10-17 — the box's own decider now fails safe too: a `.coopignore` it cannot read (link,
  folder, FIFO, no permission, over 1 MiB), in the repository or a folder above it, stops launches
  and is reported by name instead of becoming an empty rule set (task
  2026-10-17-refuse-a-coopignore-the-secret-decider-cannot-re). Swept every NewDecider and
  NewShadowDecider caller: mounts, service binds, image build context, check-secrets, init, fork
  merge, isolated publication, session review scan, presets.

- 2026-10-17 — dropped internal/box/restricted.go from sources: the read-only and bare launches are
  gone, so the ordinary launch is the only one that calls ComputeMounts.
- 2026-09-11 — approved design recorded. Swept all seven source files listed above: normal and
  restricted launches share ComputeMounts; service projection and build staging read the policy
  separately; the loader silently drops read failures; mount tests require visibility but do not
  enforce write protection. Existing implementation gaps are queued as S1 and S17 in
  2026-09-11-fix-defects-found-by-the-full-coop-feature-audit. No runtime fix or enforcement test
  exists yet, so check remains none.
