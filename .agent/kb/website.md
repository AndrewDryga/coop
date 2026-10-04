---
name: website
description: site/ is static and partly generated; tools/gen_site.py owns index.html and the docs' terminal regions, which copy real CLI output; the code-block, cache-bust and social-card traps
subsystem: website
sources: [tools/gen_site.py, tools/site/index.tpl.html, site/index.html, site/docs.html, site/assets/css/site.css, site/assets/js/site.js, tools/gen_seo_assets.py, tools/test_site_content.py, tools/align-comments.py, internal/cli/help_test.go, internal/cli/eval_cmd_test.go]
updated: 2026-10-04
---

The homepage and docs are static files in `site/`, published by `.github/workflows/pages.yml` on
every push to main. Two parts are generated and committed:

- **`site/index.html`** is rendered by `tools/gen_site.py` from `tools/site/index.tpl.html` (icons,
  provider marks from `tools/site/logos/`, terminal scenes, the commands you copy). Never edit it by
  hand: `make tools-test` runs `gen_site.py --check` (`tools/test_site_content.py`) and fails on
  drift. The task-commit count comes from `git log origin/main` when you regenerate; `--check`
  reuses the count already in the page, so CI needs no history.
- **The docs' terminal windows** fill `site/docs.html` between `<!-- gen_site: NAME -->` and
  `<!-- /gen_site -->` (`DOCS_SCENES`: check-secrets, doctor, claude, loop, fork). The rest of
  docs.html is hand-written; `--check` catches an edited or stale region, not prose edits.

**The scenes copy real CLI output**, each citing its renderer or approved transcript
(`internal/cli/testdata/approved/`, e.g. doctor is `18a-doctor-all-passed.txt`; the loop's headers
are `internal/loop/report.go`). A user-visible output change must update the matching scene and
regenerate, or the site shows output the CLI no longer prints. The old asciinema casts
(`site/casts/`) are no longer loaded by any page.

**Replays** (`SCENES.loop` in site.js; fork and doctor reuse it) are bottom-anchored 15-row windows:
commands type out, a beat's lines arrive one at a time, and each beat dwells by reading time (500ms
+ 20ms a character, at most 3s, +0.9s after a ✓). Every scene is drawn finished; site.js resets it
half a screen before it arrives and plays it once on view. Reduced motion, or a card too narrow for
its `fits` check, leaves it finished.

**Docs code blocks** (`pre.code`) start their content on the line after `<pre class="code">` (HTML
drops that newline): `tools/align-comments.py` measures from column 0, so a block starting on the
tag's own line reads as misaligned. A trailing comment is `<span class="t">` (the align tool finds
it by that exact string), a whole-line comment `<span class="t whole">`, an operator like `&&`
`<span class="o">`. On phones the CSS drops each trailing `.t` onto its own indented line, which is
why an operator must not be `.t`. Keep code lines at 65 visible characters or fewer, so nothing
wraps at 1024px.

**Tests pin docs wording.** Rewording `site/docs.html` can fail Go tests far from the site:
`internal/cli/help_test.go` TestCurrentDocsDoNotAdvertiseRetiredContracts needs "commits no starter
subagents" and rejects retired config names; `internal/cli/eval_cmd_test.go` needs the eval commands
verbatim; `tools/test_site_content.py` needs the `#evals` section and `coop down --delete-volumes`.
Run `go test ./internal/cli/` after a docs copy pass, not only `make tools-test`.

**Cache-busting.** Both pages load `assets/css/site.css` and `assets/js/site.js` with `?v=<date>`;
bump it in the template and in docs.html when either file changes (Pages caches for 10 minutes).

**The social card** (`tools/gen_seo_assets.py og`) is the homepage's own hero, rendered from a copy
with every `<script>` stripped so the sandbox picture is finished. Headless Chrome runs scripts even
with `--disable-javascript`, and `--blink-settings=scriptEnabled=false` makes `--screenshot` write
nothing. The icons are drawn from `brand/assets/coop-flat.svg`.

## Changelog
- 2026-10-04 — created with the site replacement (task 2026-10-03-settle-the-co-op-website-direction); verified against every file in sources. Added the pinned-wording trap after the docs copy pass dropped "commits no starter subagents" and failed help_test.go.
