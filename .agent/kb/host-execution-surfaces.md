---
name: host-execution-surfaces
description: which changed files count as "runs on your machine", the two tiers, and the three places coop surfaces them (fork review/merge, task flags, check-secrets)
subsystem: hostsurface
sources: [internal/hostsurface/hostsurface.go, internal/forkctl/merge.go, internal/forkctl/review.go, internal/tasks/flags.go, internal/tasks/audit.go, internal/cli/checksecrets.go]
updated: 2026-09-06
---

The box cannot run anything on the host, but a merged tree can: a hook fires on the reviewer's next
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
  review brief's "runs on your machine" section and flagged on the task board, never a blocker,
  because every repo edits its Makefile constantly.

`package.json` is deliberately NOT a path surface: `PolicyScan` keeps the content check that flags
only a NEW lifecycle script (`postinstall` etc.), so a version bump does not block a merge.
A deletion (`D` status) never counts — removing a hook cannot run anything.

The three consumers:
- `coop fork review` prints all findings; `coop fork merge` blocks on the automatic ones.
- `CompleteTrustedTask` runs `Findings` over the task's own commits and writes `flags.json` into
  the archived task folder; `Item.HasFlags` is true until `coop tasks flags <id> --ack` records
  who acknowledged it. The board, watch, and snapshot all read `HasFlags`.
- `coop check-secrets` reports the working tree's changed surfaces (staged, unstaged, untracked)
  on stderr before its verdict; it never changes the exit code, which stays the secret scan's.

## Changelog
- 2026-09-06: created with the classifier, the two tiers, and the three consumers.
