---
name: provider-session-history
description: Native session layouts, lookup bounds, and the large-history regression contract; the pre-v11 import is gone
subsystem: testing
sources: [internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/agent/session_history_large_test.go, internal/agent/history_json.go, internal/box/native_home.go, internal/box/history.go, internal/cli/resume_note.go]
updated: 2026-11-06
---

Resume is exact by provider, account, cwd, and canonical UUID. Claude checks one
`projects/<cwd-key>/<id>.jsonl` path. Codex cannot preset an ID, so it scans bounded first-line
rollout metadata for an already persisted native ID. Gemini scans version-dependent `tmp` buckets:
each accepted bucket carries a bounded absolute `.project_root` marker that exactly matches the
requested workspace. Regular JSON and JSONL chats are supported by the pinned native reader.
Streaming parsing bounds ownership fields, not the whole conversation, and requires the exact
session ID and native `sha256(cwd)` project hash. Missing/malformed markers, symlinks and
oversized ownership metadata fail closed. The cross-bucket scan remains because Gemini
has used both slug and hash bucket names.
Grok scans cwd buckets and accepts only a regular `summary.json` under the exact session directory.

Claude prints its own `claude --resume <id>` as it exits, only when that session's `.jsonl` exists
and stdout is a terminal. It runs the host's Claude, which cannot see the native home, and Coop
never sees the line because the terminal goes straight to Docker. So `coop claude` reads the
project's `projects/<cwd-key>` folder before the run and again after the box stops
(`WatchSessions`: valid-ID regular files, size and modification time only), and prints
`coop <the typed target> --resume <id>` when exactly one session changed. Several changed means
another box shares the home (a `coop loop` writes headless sessions into the same folder), and
nothing is printed. A transcript's `entrypoint` (`cli` for the terminal, `sdk-cli` for `-p`) would
tell them apart. `history.jsonl` holds only terminal prompts but is pruned and rewritten, so it is
not a safe mark. A `/clear`, or plan mode's "clear context", starts a new session mid-run, which
also leaves two changed files and so no line.

`TestSessionLookupLargeHistory` generates every native layout under disposable account roots. It
locks exact hit, full miss, wrong cwd and ID, malformed input, alternate-account isolation, and
descriptor closure for every registered provider. `BenchmarkSessionLookupLargeHistory` reports
time and allocations diagnostically; it has no machine-sensitive threshold. Do not add a secondary
index or cache unless this evidence first demonstrates a real bound the adapters cannot meet.

## Changelog
- 2026-11-06 — added the post-run resume line for `coop claude` (task
  2026-11-06-hide-claude-s-own-resume-hint-behind-a-coop-term), read against the installed Claude
  2.1.312: its exit hint code (`printResumeHint`, gated on a TTY and the transcript file) and its
  transcripts' `entrypoint` field.
- 2026-11-04 — removed the pre-v11 history import with its planners (`NativeHistory`), receipts,
  completion stamps and ACP binding migration (owner decision). A home an older build left with an
  unfinished import is refused at launch (`checkNativeHistorySettled`); history never imported stays
  in the old folders, unread. `history_json.go` stays for Gemini's live resume lookup only.
- 2026-10-19 — fork records now live in `forkspace.StateDir` (Coop's private folder); a stamp
  taken before the move no longer matches, so the next launch plans the import once more.
- 2026-10-15 — added completion stamps so launches stop re-planning finished pre-v11 imports
  (task 2026-10-15-stop-rescanning-pre-v11-history-on-every-launch); reverified the planners and
  receipts against the sources above.
- 2026-10-09 — reverified four-provider adapters and pinned Gemini JSON/JSONL reader support.
  Ordinary complete homes are repository/provider/account scoped; ACP homes are account-independent,
  including remote private roots. Validated forks share their parent domain. Streaming imports keep
  original transcripts and require exact cwd ownership. Source/destination leases and writer checks
  fence publication. Receipts outside the writable home prevent resurrection of deleted transcripts
  and consumed index rows; before/after digests recover interrupted index publication without
  overwriting changed native state. Gemini rebuilds projects.json from .project_root markers, so
  no cross-project registry is merged. ACP binding tombstones preserve native deletions too.
- 2026-09-03 - removed absent whole-file JSON and markerless/hash-bucket fallbacks after verifying
  Gemini 0.50.0 state across every managed profile and the ACP session store; retained current
  marker, JSONL metadata, multi-bucket, bounds, symlink, account, and descriptor-leak coverage
- 2026-07-16 - created from the four-provider large-history investigation and Gemini bounded-scan fix
