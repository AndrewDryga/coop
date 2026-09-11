---
name: host-execution-surfaces
description: which changed files count as "runs on your machine", the two tiers, and the two places coop surfaces them (fork review/merge, check-secrets)
subsystem: hostsurface
sources: [internal/hostsurface/hostsurface.go, internal/forkctl/merge.go, internal/forkctl/review.go, internal/cli/checksecrets.go]
updated: 2026-09-11
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
  review brief's "runs on your machine" section, never a blocker, because every repo edits its
  Makefile constantly.

`package.json` is deliberately NOT a path surface: `PolicyScan` keeps the content check that flags
only a NEW lifecycle script (`postinstall` etc.), so a version bump does not block a merge.
A deletion (`D` status) never counts — removing a hook cannot run anything.

The two consumers:
- `coop fork review` prints all findings; `coop fork merge` blocks on the automatic ones.
- `coop check-secrets` reports the working tree's changed surfaces (staged, unstaged, untracked)
  on stderr before its verdict; it never changes the exit code, which stays the secret scan's.

There is no third, task-side consumer: the task-flags feature (`flags.json`, `Item.HasFlags`,
`coop tasks flags --ack`) was removed outright on 2026-09-11, so completion writes no record and
the board carries no acknowledgement marker. Old `flags.json` files are inert: nothing reads,
writes or deletes them. Review agent work at the fork boundary instead.

## Changelog
- 2026-09-11: task-flags consumer removed with the feature; card is the classifier + its two
  remaining consumers.
- 2026-09-06: created with the classifier, the two tiers, and the three consumers.
