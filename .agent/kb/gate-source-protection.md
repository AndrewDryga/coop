---
name: gate-source-protection
description: Built-in gate files plus a frozen project-owned exact-path list decide which loop changes require protected review
subsystem: loop
sources: [internal/project/project.go, internal/tasks/audit.go, internal/loop/loop.go, internal/loop/changes.go, internal/scaffold/projectfile.go]
updated: 2026-09-15
---

Coop always protects its fixed Makefile, CI, hook, and agent-config paths. A project can add the
exact wrapper scripts or source files that implement its checker with top-level `gate_sources` in
`.agent/project.yaml`. Matching is exact: the project owns this finite source-closure statement;
Coop does not guess dependencies from names or imports.

The loop loads and validates the list once after taking the checkout lock, before any worker runs.
That same frozen list drives iteration telemetry, the mandatory protected audit, and later review
context. Editing or removing the declaration cannot erase protection during the current run;
`.agent/project.yaml` is itself a built-in protected file. A legitimate declaration change takes
effect when the loop restarts. A missing list preserves the prior built-in-only behavior.

## Changelog
- 2026-09-15 — added the explicit project source list and startup snapshot contract.
