---
name: replacements-remove-old-paths
description: "simplification removes obsolete configuration, adapters and callers in the same change"
scope: architecture
sources: [internal/cli/session_cmd.go, internal/cli/session_connect.go, internal/sessionsvc/service.go, internal/workerconnector/executor.go, internal/workerproto/protocol.go]
check: none
updated: 2026-09-27
---

# Replacements remove the old path

A request to simplify an existing feature is not permission to add a parallel implementation.
Name what disappears, migrate every caller, and delete its configuration, dispatch, adapters,
tests and documentation in the same change. Do not leave compatibility routes for a pre-v1
consumer unless the user explicitly requires them. Preserving historical data does not require
preserving obsolete execution authority.

**Why:** the user requested one worker connection workflow and removal of local policies, then
found an additive change retaining the old machinery. Count the whole diff, including new
untracked files; distinguish production code, tests and docs. Line count alone is not the bar:
one authority model and one execution path must remain.

Generic integration belongs at the existing API boundary. Product-specific workflow decisions
belong in the controller, not a second command vocabulary inside Coop.

## Changelog
- 2026-09-27 — rechecked the five source files and current caller/docs searches: worker JSON,
  public serve/policies, local policy admission and specialized wire commands are gone. Historical
  data decoding remains read-only; source/publication and generic body transport share one job path.
- 2026-09-26 — swept the five source files. Found parallel job/policy admission, a YAML policy
  registry, worker JSON configuration, public serve/policies commands and per-method controller
  translation. Removal and caller migration are part of the active worker cutover task; the
  sweep is not evidence of completion.
