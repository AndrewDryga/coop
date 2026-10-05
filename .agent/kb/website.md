---
name: website
description: site/ is static and partly generated; tools/gen_site.py owns index.html and the docs' terminal regions, which copy real CLI output; the code-block, cache-bust and social-card traps
subsystem: website
sources: [tools/gen_site.py, tools/site/index.tpl.html, site/index.html, site/docs.html, site/assets/css/site.css, site/assets/js/site.js, tools/gen_seo_assets.py, tools/test_site_content.py, tools/align-comments.py, internal/cli/help_test.go, internal/cli/eval_cmd_test.go]
updated: 2026-10-05
---

The homepage and docs are static files in `site/`, published by `.github/workflows/pages.yml` on
every push to main. Two parts are generated and committed:

- **`site/index.html`** is rendered by `tools/gen_site.py` from `tools/site/index.tpl.html` (icons,
  provider marks from `tools/site/logos/`, terminal scenes, the commands you copy). Never edit it by
  hand: `make tools-test` runs `gen_site.py --check` (`tools/test_site_content.py`) and fails on
  drift. The task-commit count adds up the Coop-Task commits on `origin/main` of co:op, Emisar
  (`../emisar`) and Ryker (`../responder`) when you regenerate, so fetch all three first; a missing
  checkout stops the generator. `--check` reuses the count already in the page, so CI needs no
  history.
- **The docs' terminal windows** fill `site/docs.html` between `<!-- gen_site: NAME -->` and
  `<!-- /gen_site -->` (`DOCS_SCENES`: check-secrets, doctor, claude, loop, fork). The rest of
  docs.html is hand-written; `--check` catches an edited or stale region, not prose edits.

**The scenes copy real CLI output**, and `TERMINAL_SOURCES` in `gen_site.py` says where each
terminal's lines come from. A transcript block (doctor is `18a-doctor-all-passed.txt`) must use
only lines of that approved file. Any other block cites the Go files that print it and the shape of
each line (`{}` is a value), and each shape's literal parts must still be in those files; pure
example values such as task titles are listed. `terminal_drift` checks both pages through
`gen_site.py --check` and `tools/test_site_content.py`, naming the page, block and line. So a CLI
wording change fails make check until the scene is updated, and so does a scene line the CLI never
prints. A new terminal needs an entry, or the check names it.

**Replays** (`SCENES.loop` in site.js; fork and doctor reuse it) are bottom-anchored 15-row windows:
commands type out, a beat's lines arrive one at a time, and each beat dwells by reading time (500ms
+ 20ms a character, at most 3s, +0.9s after a ✓). Every scene is drawn finished; site.js resets it
half a screen before it arrives and plays it once on view. Reduced motion, or a card too narrow for
its `fits` check, leaves it finished.

**Docs commands and files.** A shell command a reader copies is a row in a command list:
`<ul class="cmds" role="list"><li><code data-copy-source>…</code><span class="cmd-note"># …</span></li>`.
site.js gives every row (and every `.code-file`) the homepage's Copy button, so a button copies one
command, never a block: pasting a whole block would run unrelated commands, `coop down
--delete-volumes` included. A note sits beside its command, so a block's widest command plus any
note must stay within 57 characters (else it wraps at 1024px); `tools/test_site_content.py` checks
that, that every command parses with `sh -n`, and that no `<pre class="code">` holds a shell command.
A file snippet is `<div class="code-file">` with a `<p class="code-file-name">` header over a
`<pre class="code" data-copy-source>`, copied whole. Inside those `pre` blocks, content starts on the
line after the tag (HTML drops that newline; `tools/align-comments.py` measures from column 0), a
trailing comment is `<span class="t">` (the align tool finds it by that exact string) and a
whole-line comment `<span class="t whole">`.

**The docs contents fold.** site.js turns each `.side-group` label into a button (`aria-expanded`,
`aria-controls`) over its `hidden` list and keeps one group open: the one holding the section being
read, until you open another by its name. Without the script every group stays open. Keep the
expanded contents under a 1280x700 window when adding sections or groups.

**The docs' type and rhythm** are one scale in the Docs block of `site.css`: prose 18px on 32px
lines in a 42rem column (about 68 characters a line), a section's opening paragraph included (the
owner rejected a larger lead as "larger, smaller, larger"); subheadings 24px; code, tables
and the interface 16px. Spacing comes from flow rules on `.doc-section > *`: 24px between prose,
32px around every block, 48px over and 16px under a subheading. A new kind of block joins the
`:where(...)` list there, or it sits 24px from its neighbours like prose; components set their own
`font`, so never style `.doc-section p, li` broadly (that once made command notes 18px).

**Figures.** Every number on the homepage is an `.evidence` line: the figure in `<strong>`, then one
sentence that links its source and claims only what the source says. A figure about one attack or
mistake is the first child of that case's `.attack`, above its `.case` block (the threat and its
stopped lines), never inside it: the owner rejected a figure between a threat and its stopped lines.
Listed (below 1024px or without the script), each figure heads its case over a block with its own
line. Pinned, site.js copies the figures into a `.case-figures` slot above the rail and shows the one
for the case on screen. A chapter gives every case a figure or none (site.js builds the slot only
then, and a site test checks it): the owner rejected the empty gap a figureless case left. Never put
figures on a timer, and never a figure whose sentence claims more than its source. The
"3 s" start in the first story step is our own number: `make lifecycle-bench` (filtered_start, 10
samples) on a clean clone, linked to `tools/lifecycle_bench.py`. Measure again on an idle machine
before changing it, and never in a checkout with heavy local state, which starts slower.

**The Zed card is a real screenshot** (`site/assets/img/zed-coop.png`): the window inside its 1px
border, cut from a retina capture (`magick … txt:-` finds the border rows and columns) at even
dimensions, so it shows at exactly half its pixel size and is never stretched. It sits on a surface
card whose padding is the same on all four sides, and the 12px radius hides the window's rounded
corners, which still hold desktop pixels. A new capture follows the same rule.

**Tests pin docs wording.** Rewording `site/docs.html` can fail Go tests far from the site:
`internal/cli/help_test.go` TestCurrentDocsDoNotAdvertiseRetiredContracts needs "commits no starter
subagents" and rejects retired config names; `internal/cli/eval_cmd_test.go` needs the eval commands
verbatim; `tools/test_site_content.py` needs the `#evals` section and `coop down --delete-volumes`.
Run `go test ./internal/cli/` after a docs copy pass, not only `make tools-test`.

**Cache-busting.** Both pages load `site.css`, `site.js` and `analytics.js` with `?v=<hash of the
three>`, written by `tools/gen_site.py`, so `--check` fails after any CSS or JS edit until you
regenerate. (A fixed date once left the owner looking at a stale stylesheet all afternoon.)

**The social card** (`tools/gen_seo_assets.py og`) is the homepage's own hero, rendered from a copy
with every `<script>` stripped so the sandbox picture is finished. Headless Chrome runs scripts even
with `--disable-javascript`, and `--blink-settings=scriptEnabled=false` makes `--screenshot` write
nothing. The icons are drawn from `brand/assets/coop-flat.svg`.

## Changelog
- 2026-10-04 — created with the site replacement (task 2026-10-03-settle-the-co-op-website-direction); verified against every file in sources. Added the pinned-wording trap after the docs copy pass dropped "commits no starter subagents" and failed help_test.go.
- 2026-10-04 — shell commands became copyable command lists and file snippets got name headers (task 2026-10-04-make-the-docs-read-as-one-guide-with-a-copy-butt); the code-block paragraph now describes both.
- 2026-10-04 — the docs contents fold to the group being read (task 2026-10-04-show-services-isolation-link-ryker-and-protector).
- 2026-10-04 — the docs' type scale and rhythm (task 2026-10-04-give-the-docs-one-type-scale-and-a-steady-vertic).
- 2026-10-04 — the asciinema casts, player and cast tools are gone (task 2026-10-04-retire-the-asciinema-cast-pipeline-the-old-websi).
- 2026-10-04 — asset links carry a content hash instead of a hand-bumped date (task 2026-10-04-bust-the-browser-cache-whenever-the-site-s-css-o).
- 2026-10-04 — each homepage case carries its own sourced figure (task 2026-10-04-give-each-homepage-case-its-own-sourced-figure-a).
- 2026-10-04 — the measured warm start in story step 1 (task 2026-10-04-show-how-fast-a-warm-box-starts-on-the-homepage).
- 2026-10-04 — terminals are checked against their CLI sources (task 2026-10-02-fail-the-docs-check-when-a-site-output-snippet-d).
- 2026-10-05 — case figures sit above their case's block, never inside it (task 2026-10-05-keep-each-homepage-figure-above-its-case-block-n).
- 2026-10-05 — every case in a chapter has a figure or none; Presets carries a figure (task 2026-10-05-give-the-push-case-a-figure-and-presets-a-multi).
- 2026-10-05 — the Zed card is a real screenshot (task 2026-10-05-show-a-real-zed-screenshot-in-integrations).
