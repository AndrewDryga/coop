---
name: release-qualification
description: "tagged releases reuse exact-commit CI, validate finalized notes and preserve tag identity"
subsystem: release
sources: [.github/workflows/ci.yml, .github/workflows/release.yml, .goreleaser.yaml, tools/release_preflight.py, release_test.go]
updated: 2026-09-06
---

Release `qualify` locally calls ci.yml from the event commit; its canonical gate, Docker/Podman
doctor and Docker review-write jobs must succeed before the privileged publisher starts.
Every checkout explicitly uses github.sha. Qualification remains contents-read only.

The preflight helper compares the event tag's peeled commit to HEAD and extracts the first real
ATX H2 section of CHANGELOG.md. Its heading must exactly match the SemVer tag without `v`;
Unreleased, empty notes, Setext release headings, raw HTML blocks and unclosed comments/fences
refuse. Code examples stay literal, comments outside code are removed, and failures emit no partial notes. This is a
release-format validator, not a general Markdown renderer.

GoReleaser2.16.0 prefers GORELEASER_CURRENT_TAG over other tags at HEAD, so the workflow pins it
to the validated event tag. Its changelog pipe must remain enabled to consume --release-notes.
Its dirty-tree check precedes before hooks: `go mod tidy -diff` refuses graph drift without
rewriting the files after qualification; the completions hook remains generated from source.

The remote raw/peeled tag recheck runs just before GoReleaser starts, not atomically with GitHub
publication. Keep v* update/deletion rules enforced; trusted bypass actors must not mutate the
tag or protection during build/publication. Local fixture tests cannot prove hosted CI, signing
or publication succeeded. Test refs/remotes are confined to temporary filesystem repositories.

## Changelog
- 2026-09-06 — created from M01; verified workflow dependencies, tag/notes helper fixtures and
  pinned GoReleaser pipeline ordering. No repository settings change or public tag probe.
