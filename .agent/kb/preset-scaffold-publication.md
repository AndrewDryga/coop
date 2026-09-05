---
name: preset-scaffold-publication
description: preset initialization publishes one validated create-only bundle and preserves incomplete destinations
subsystem: presets
sources: [internal/preset/scaffold.go, internal/preset/preset.go, internal/preset/scaffold_rename_linux.go, internal/preset/scaffold_rename_darwin.go]
updated: 2026-09-05
---

# A preset scaffold is one create-only directory

`Scaffold` refuses any existing destination entry, not just an existing `preset.yaml`.
Incomplete prompt-only directories belong to the user too. Parent directories are pinned and
must be real directories; existing permissions are never changed.

The complete starter bundle is written and synced inside a private wrapper under the pinned preset
parent. The wrapper contains `bundle/`, never a direct `preset.yaml`: `List` otherwise discovers
even hidden directories, and a requested preset can itself be named `preset.yaml`. Interrupted
wrappers stay invisible and do not prevent another attempt; cleanup touches only the current stage.
Cleanup records created file/directory identities and uses pinned parent handles, including the
roles directory. Replaced children and unknown additions remain as residue rather than authorizing
recursive deletion. These substitution checks are not isolation from arbitrary same-user host edits.

Validation shares the ordinary preset parser but reads through the staged root, without reopening
mutable parent paths. Normal `Load` retains its existing path resolution and prompt semantics.
Darwin/Linux publication uses directory-descriptor-relative no-replace rename, so even a concurrent
empty destination cannot be replaced. A sync error after publication reports that the preset was
published and never deletes it as rollback.

## Changelog
- 2026-09-05 — created after reproducing prompt truncation and symlink-parent writes; mapped
  rooted staged validation, atomic collision refusal, interrupted retry and publication boundaries.
