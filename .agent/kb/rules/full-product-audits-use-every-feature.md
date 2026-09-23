---
name: full-product-audits-use-every-feature
description: a full product audit manually exercises each advertised supported feature and labels unrun paths honestly
scope: agent-workflow
sources: [AGENTS.md, README.md, internal/cli/help.go]
check: none
updated: 2026-09-23
---

# Manually use every feature before calling a full product audit complete

For a requested full product audit, enumerate advertised supported commands and distinct user
journeys, then operate each through the real product on every platform the audit claims. Record the
exact build, invocation, result, practical usability observation and a meaningful denial or recovery
path where it matters. A family-level inventory, source reading, mocked test or cross-build is not
manual feature proof. Mark paid, unavailable, unsupported and human-only destructive paths explicitly
instead of silently counting them as passed.

**Why:** the human corrected the Coop campaign after a green gate and feature-family ledger were
described too broadly: "I want this to include manual testing of every feature too, that will make
sure they all work as expected, they are PRACTICAL for users, and have good usability."

**How to apply:** crosswalk `coop help --all`, README promises and the audit ledger to a per-feature
manual matrix. Exercise safe supported paths in disposable repositories, with terminal and
redirected output where relevant. Keep real secrets out of artifacts, retain a precise blocker
where another operator or environment is required, and run the paid qualification last if already
authorized. Fix only observed practical defects; do not create endless speculative edge cases.

## Changelog

- 2026-09-23 — created after comparing 72 manual acceptance rows with the prior campaign's 16
  feature-family rows and all current `coop help --all` sections. Existing automated and source-only
  evidence remains labeled as such; the active manual-qualification task owns the missing hands-on
  checks. Swept the current active/blocked full-product campaign and this rule's three source files;
  no other active full-product audit asserted a manual-every-feature pass. Review-only check.
