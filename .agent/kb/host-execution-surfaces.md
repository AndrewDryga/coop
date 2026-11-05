---
name: host-execution-surfaces
description: local execution classifications, fork protections and controller-authorized CI publication
subsystem: hostsurface
sources: [internal/hostsurface/hostsurface.go, internal/forkctl/merge.go, internal/forkctl/review.go, internal/forkctl/isolated_candidate.go, internal/cli/checksecrets.go, internal/sessionsvc/review_scan.go, internal/scaffold/stock.go]
updated: 2026-11-04
---

Shared-write boxes can plant metadata that native host tools execute even before a merge,
including new nested Git repositories. Coop hardens its own host Git, not arbitrary native tools.
Optional isolated forks add controlled publication ([[isolated-fork-publication]]). A tree can
carry host execution: a hook fires on the reviewer's next
`git commit`, `.envrc` on `cd`, `.claude/settings.json` hooks on the next Claude session, an MCP
config on the next editor start, `.agent/compose.yml` on the next box start, a CI workflow on push,
the Makefile on `make`. `internal/hostsurface` is the ONE classifier for those paths; every place
that reviews agent work reads it, so a new surface is added there and nowhere else.

Two tiers, decided by `Finding.Automatic`:
- **automatic** — runs without a deliberate command (hooks, `.envrc`, `.gitattributes` filters,
  `.gitmodules`, pre-commit config, `.claude/`, `.codex/`, `.gemini/` settings and hooks, MCP
  configs, `.agent/` skills, compose, Dockerfile, project/loop config, `.vscode`/`.zed`/`.idea`
  task and settings files, `.github/workflows/`). `PolicyScan` turns these into merge blockers
  (`coop fork merge` refuses without `--force`).
- **deliberate** — runs only when the human runs it (Makefile, justfile, Taskfile). Listed in the
  review brief's "runs on your machine" section, never a blocker, because every repo edits its
  Makefile constantly.

Each finding's `Reason` is ONE sentence naming when the file runs — "Runs during Git operations.",
"Runs when you use make." — because it is printed under the path in `coop check-secrets` and in the
fork review brief, where a clause like "runs on your machine on `git commit`" read as a fragment of
somebody else's sentence. Change the wording here and every consumer follows; the TIER
(`Automatic`), not the wording, is what decides whether a merge blocks.

`package.json` is deliberately NOT a path surface: `PolicyScan` keeps the content check that flags
only a NEW lifecycle script (`postinstall` etc.), so a version bump does not block a merge.
A deletion (`D` status) never counts — removing a hook cannot run anything.

Isolated publication scans every incoming immutable tree, not only the final diff. It retains
these path/content risk checks, denies changed gitlinks/.gitmodules, and offers no force bypass.

Controller-authorized session publication uses `PublicationBlocker`, which permits CI-only
edits while preserving every local automatic surface. It still scans the workflow bytes for
credentials and checks hidden files and package lifecycle changes. A hook nested under workflows
is classified before CI and remains blocked. The controller's exact-candidate publication grant
is the authority; ordinary task CI edits need no separate human review. Local fork protections
keep using `Classify` unchanged.

The consumers:
- `coop fork review` prints all findings; `coop fork merge` blocks on the automatic ones.
- `coop check-secrets` reports the working tree's changed surfaces (staged, unstaged, untracked)
  under `Review files that run commands`, AFTER its verdict; it never changes the exit code, which
  stays the secret scan's. A file whose bytes (or link target) are exactly what this Coop's init
  writes at that path (`scaffold.Stock`: templates, the hook and gate for every language choice,
  project/Compose files for every service choice, the Dockerfile for the current .tool-versions,
  init's links) is counted in one line instead of listed: a fresh project's sixteen setup files
  buried its secret result. One changed byte lists it again; fork review/merge keep listing all.
- Session review refuses local execution and content findings before retaining the exact
  candidate for controller-authorized publication.

## Changelog
- 2026-11-04: dropped the paragraph on the removed task-flags feature. The 2026-09-11 entry below
  records the removal, and MIGRATING.md tells a user that an old `flags.json` is left in place.
- 2026-11-03: check-secrets counts Coop's unchanged setup files instead of listing them
  (`scaffold.Stock`, proven against every file init writes by TestStockMatchesEveryFileInitWrites).
- 2026-10-30: separated authorized remote CI publication from local fork execution policy;
  swept the classifier, session scanner, fork merge and isolated candidate consumers. Local
  protections are unchanged; publication and scanner regressions cover the distinction.
- 2026-10-08: qualified isolated history scanning and corrected the shared-write host-execution
  assumption; ordinary policy still allows explicit --force, isolated publication does not.
- 2026-09-11: task-flags consumer removed with the feature; card is the classifier + its two
  remaining consumers.
- 2026-09-11: reasons became one-sentence statements ("Runs during Git operations."); check-secrets
  prints them under `Review files that run commands` after its verdict. Verified against
  hostsurface.go's Classify table and internal/cli/checksecrets.go.
- 2026-09-06: created with the classifier, the two tiers, and the three consumers.
