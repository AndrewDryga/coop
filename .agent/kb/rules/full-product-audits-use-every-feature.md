---
name: full-product-audits-use-every-feature
description: a full product audit manually exercises each advertised supported feature and labels unrun paths honestly
scope: agent-workflow
sources: [AGENTS.md, README.md, internal/cli/help.go, internal/cli/rotation.go]
check: none
updated: 2026-09-26
---

# Manually use every feature before calling a full product audit complete

For a requested full product audit, enumerate advertised supported commands and distinct user
journeys, then operate each through the real product on every platform the audit claims. Record the
exact build, invocation, result, practical usability observation and a meaningful denial or recovery
path where it matters. A family-level inventory, source reading, mocked test or cross-build is not
manual feature proof. Mark paid, unavailable, unsupported and human-only destructive paths explicitly
instead of silently counting them as passed.
Before repeating an old `not_configured` or quota-blocked finding, recheck current account and
limit state. If a real exhausted account and a working sibling now exist, exercise one bounded
automatic-failover journey; do not infer it from two sign-in markers alone.

**Why:** the human corrected the Coop campaign after a green gate and feature-family ledger were
described too broadly: "I want this to include manual testing of every feature too, that will make
sure they all work as expected, they are PRACTICAL for users, and have good usability."
Later, the human corrected a stale statement that two-account failover had to wait: "but we have
accounts signed in that are not rate limited!" The current Codex default was genuinely limited,
while the second account completed a one-task loop after automatic rotation.

**How to apply:** crosswalk `coop help --all`, README promises and the audit ledger to a per-feature
manual matrix. Exercise safe supported paths in disposable repositories, with terminal and
redirected output where relevant. Keep real secrets out of artifacts, retain a precise blocker
where another operator or environment is required, and run the paid qualification last if already
authorized. For failover, inspect `coop credentials`, keep account selection unpinned so the
ladder can expand, and record both the limited first attempt and successful continuation. Scope
the claim to the provider, client, platform and command family actually exercised. Fix only
observed practical defects; do not create endless speculative edge cases.

## Changelog

- 2026-09-26 — added a current-preconditions check after a real Codex `personal` limit rotated a
  bounded loop to working `emisar` and completed one disposable task. Swept the archived campaign
  ledger, its manual follow-up and the lifecycle MCP note: the older `not_configured` rows describe
  their then-current record, so a dated follow-up corrects present status without rewriting history.
  Compared the live run with the account-expansion path in `internal/cli/rotation.go`; no other
  active audit task carried this stale blocker. Review-only check remains `none`.
- 2026-09-23 — created after comparing 72 manual acceptance rows with the prior campaign's 16
  feature-family rows and all current `coop help --all` sections. Existing automated and source-only
  evidence remains labeled as such; the active manual-qualification task owns the missing hands-on
  checks. Swept the current active/blocked full-product campaign and this rule's three source files;
  no other active full-product audit asserted a manual-every-feature pass. Review-only check.
