---
name: claude-first-run-answers
description: Claude drops the bypass acceptance from .claude.json on its first start and never knew a new fork's folder, so Coop refills both before every launch and into the project's settings fallback
subsystem: box
sources: [internal/agent/claude.go, internal/agent/agent.go, internal/box/native_home.go, internal/box/run.go]
updated: 2026-11-06
---

Claude shows two prompts that exit by default: the bypass-permissions warning and the "do you trust
this folder" check. Coop answers both in files, so neither may ever appear in a box.

- **Bypass warning.** The dialog is skipped when `skipDangerousModePermissionPrompt` is true in
  Claude's user settings (`$CLAUDE_CONFIG_DIR/settings.json`) or the old `bypassPermissionsModeAccepted`
  is true in `.claude.json`. On its first start Claude migrates the second into the first and
  deletes it from `.claude.json` (read out of the 2.1.312 binary; the migration runs once, until
  `migrationVersion` reaches its current number). Accepting the dialog writes the settings key too.
- **Trust.** `projects[<folder>].hasTrustDialogAccepted` in `.claude.json`, for the box's working
  folder or one of its parents up to the git root.

Seeding only a new home (`EnsureDefaults`, from `prepareNativeHome`) therefore held for exactly one
launch. After Claude's first start the acceptance was gone from `.claude.json`, and a project that
commits `.agent/claude/settings.json` has that file copied over the home's `settings.json` inside the
box (`HomeFallbacks`), which hides the seeded settings key; accepting writes to that throwaway copy.
Every fork also shares its project's home (`HistoryKey` follows the fork binding), so a new fork's
folder was never trusted. Scratch repos without `coop init` never showed it: no fallback, so the home's
own settings carried the key.

What Coop does now, both without writing into the repo:

- `claudeAgent.RefreshDefaults` (the optional `agents.DefaultsRefresher`) runs for a published home
  at every launch, under the home's lock in `prepareNativeHome`. It fills only the missing
  `.claude.json` answers for this launch's folder and writes only when one is missing.
- A file fallback with a `Fill` hook (Claude's settings) is copied with the same settings answers
  added, every value the file sets kept (`fillClaudeSettings`); a file that is not a JSON object is
  copied as it was.

The check order and the migration are Claude's, so they can change: the refill is idempotent, so a
Claude that moves the key again is answered again at the next launch.

## Changelog
- 2026-11-06 — created for the QA finding that every Claude launch after the first met the bypass
  warning (and each new fork the trust prompt) in a `coop init` repo; mechanism read out of the
  installed 2.1.312 client's startup code, not run, since the dialogs come before any model call
  (task 2026-11-06-stop-claude-s-bypass-and-trust-prompts-from-comi).
