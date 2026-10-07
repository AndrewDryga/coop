---
name: website
description: site/ is static and partly generated; tools/gen_site.py owns index.html, overnight.html and the docs' terminal regions, which copy real CLI output; the night board, code-block, cache-bust and social-card traps
subsystem: website
sources: [tools/gen_site.py, tools/services_bench.py, tools/site/index.tpl.html, tools/site/overnight.tpl.html, site/index.html, site/overnight.html, site/docs.html, site/assets/css/site.css, site/assets/css/overnight.css, site/assets/js/site.js, site/assets/js/overnight.js, tools/gen_seo_assets.py, tools/test_site_content.py, tools/align-comments.py, internal/cli/help_test.go, internal/cli/eval_cmd_test.go]
updated: 2026-10-07
---

The homepage and docs are static files in `site/`, published by `.github/workflows/pages.yml` on
every push to main. Two parts are generated and committed:

- **`site/index.html`** is rendered by `tools/gen_site.py` from `tools/site/index.tpl.html` (icons,
  provider marks from `tools/site/logos/`, terminal scenes, the commands you copy). Never edit it by
  hand: `make tools-test` runs `gen_site.py --check` (`tools/test_site_content.py`) and fails on
  drift. The task-commit count adds up the commits carrying a Coop-Task trailer (a squash commit with
  several counts once) on `origin/main` of co:op, Emisar
  (`../emisar`) and Ryker (`../responder`) when you regenerate, so fetch all three first; a missing
  checkout stops the generator. `--check` reuses the count already in the page, so CI needs no
  history. A change that is not about the count (copy, layout) restores `site/index.html` after
  regenerating: a new count is its own commit, made after fetching all three.
- **The docs' terminal windows** fill `site/docs.html` between `<!-- gen_site: NAME -->` and
  `<!-- /gen_site -->` (`DOCS_SCENES`: check-secrets, doctor, claude, loop, fork). The rest of
  docs.html is hand-written; `--check` catches an edited or stale region, not prose edits.

**The overnight page** (`site/overnight.html`, from `tools/site/overnight.tpl.html`, with its own
`overnight.css` and `overnight.js`) is the second landing, for people who want agents working
through many tasks unattended; the homepage stays the sandbox story and the main one. A pinned
`coop loop` board lives through one night, 22:00 to 07:00, beside its steps. `night_board()` in
`gen_site.py` draws it at morning, so the page reads whole without the script; `NIGHT_TIMELINE`
holds one state for the hero and one per step (a site test keeps the counts equal: add a state
with a step). overnight.js plays a step forward in beats: the task in hand finishes in its card
(the ring closes, the tick draws, the commit types out), the decision folds or opens (`ask`), the
night time-lapses (clock, dots, counts), then the step's own event (the account handoff, the review
turning a finished task back). Scrolling back, or reduced motion, jumps straight to the step, and a
new scroll cancels the beats still to come. Owner rules from its rounds: its figures never repeat
the homepage's and each is re-checked in its primary source; the decision is one line except on
its own step and the morning; both accounts stay in view side by side; one row of dots, each three
tasks; the phone board keeps one fixed height, so measure the natural heights at 360-414 px and on
a tablet before adding content. Its Project services figure (6.5 s) is our own number:
`tools/services_bench.py`, the launch that starts Postgres and Redis (see
[[lifecycle-latency-measurement]]); measure again on a quiet machine before changing it.
Traps: Chrome drops a row's computed style when it moves between lists, so CSS transitions never
fire there and state changes are drawn with the Web Animations API; a multi-step keyframe sequence
must run on a linear clock with the easing on each keyframe (an expo-out effect easing squeezed the
handoff into its first frames); the board's height animates with what moves inside it, or rows
slide past its edge. Its sandbox replays the homepage's four layers once on view, with
`.night-sandbox.armed:not(.sN)` copying the homepage's `.stage:not(.sN)` gates, and is zoomed to its
column on narrow phones. Every page's menu lists Sandbox, Overnight, Features, Docs, GitHub and
Install; phones keep Overnight, Docs and Install (tighter at 400 px, the mark alone at 340 px).
The agent picker rewrites `data-cmd` login, run and loop; a test holds every page's start
commands to what site.js handles.

**The scenes copy real CLI output**, and `TERMINAL_SOURCES` in `gen_site.py` says where each
terminal's lines come from. A transcript block (doctor is `18a-doctor-all-passed.txt`) must use
only lines of that approved file. Any other block cites the Go files that print it and the shape of
each line (`{}` is a value), and each shape's literal parts must still be in those files; pure
example values such as task titles are listed. `terminal_drift` checks both pages through
`gen_site.py --check` and `tools/test_site_content.py`, naming the page, block and line. So a CLI
wording change fails make check until the scene is updated, and so does a scene line the CLI never
prints. A new terminal needs an entry, or the check names it.

**Replays** (`SCENES.loop` in site.js; fork and doctor reuse it) are 15-row windows that fill from
the top once staged and then scroll to keep the newest line, as a terminal does (the owner found
a window that filled from the bottom unnatural); drawn finished, they show the last 15 rows.
Commands type out, a beat's lines arrive one at a time, and each beat dwells by reading time (500ms
+ 20ms a character, at most 3s, +0.9s after a ✓). Every scene is drawn finished; site.js resets it
half a screen before it arrives and plays it once on view. Reduced motion, or a card too narrow for
its `fits` check, leaves it finished.

**Small screens play the story as a deck** (`.js-deck`, set by the head script and site.js below
1024px when the window is at least 500px tall). The stage sticks to the top (`--frame`: 42svh on a
phone, 50svh on a tablet, 36svh on a short screen), and every step, chapter opening (site.js wraps
its h2 and first p in `.deck-intro`, only in this mode) and attack is a sticky card under it; the
last card whose top has reached the stage's floor sets the stage state, through the same
`:is(.js-story, .js-deck) .stage…` rules as desktop. A camera (`aim()` in site.js) transforms the
scene to frame the rows a card is about, measured with offsetLeft/offsetTop so the transform does
not skew them. Every card is sticky at `min(--frame, --screen - --h)`, `--h` being its measured
height: a card that fits stops under the stage, and a taller one stops once its last line is on
screen, so the deck never has a card that just scrolls by (the owner saw that as broken).
Steps and chapter openings are full-width sheets; an attack is a raised card set in from the edges,
so it reads as part of its chapter (the owner found equal-looking cards hid which was the section).
Its `.deck-ticks` (added by site.js in this mode) count which case it is: the deck's only progress
marks, as the rail's ticks are on wide screens (an overall strip under the picture was one too many).
Desktop's layout rules stay `.js-story`-only: a deck change must leave 1024px and up pixel-identical
(diff against `git archive HEAD site` with the commit count matched).

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
figures on a timer, and never a figure whose sentence claims more than its source. The Zed
figure (Integrations) comes from zed.dev/agent-metrics, a live 30-day page: read it again
before you touch that line, and use today's number. The Loops figure (120 tasks in one
9-hour run during our normal work, as the owner asked it to say) is the owner's own number from a
run this machine's git and loop telemetry don't hold; ask the owner before you change it. The
"3 s" start in the first story step is our own number: `make lifecycle-bench` (filtered_start, 10
samples) on a clean clone, linked to `tools/lifecycle_bench.py`. Measure again on an idle machine
before changing it, and never in a checkout with heavy local state, which starts slower.

**The Zed card is a real screenshot** (`site/assets/img/zed-coop.png`): the window inside its 1px
border, cut from a retina capture (`magick … txt:-` finds the border rows and columns). Its two
rounded window corners are repainted in the bar color, so no desktop pixels show however the page
rounds it. It fills the Integrations column alone, with no frame or outline (the owner found the
framed version ugly, and had the `coop sessions connect` terminal under it removed). A new capture
follows the same rule.

**The Get started commands** each put their label (Install, Sign in…) above the command in the 14px
supporting size, with the Copy button level with the command. The label spans the row
(`grid-column: 1 / -1`); without that, the grid places the command beside the label, not under it.

**Tests pin docs wording.** Rewording `site/docs.html` can fail Go tests far from the site:
`internal/cli/help_test.go` TestCurrentDocsDoNotAdvertiseRetiredContracts needs "commits no starter
subagents" and rejects retired config names; `internal/cli/eval_cmd_test.go` needs the eval commands
verbatim; `tools/test_site_content.py` needs the `#evals` section and `coop down --delete-volumes`.
Run `go test ./internal/cli/` after a docs copy pass, not only `make tools-test`.

**Cache-busting.** Every page loads its stylesheets and scripts with `?v=<hash of site.css,
site.js, analytics.js, overnight.css and overnight.js>`, written by `tools/gen_site.py`, so `--check`
fails after any CSS or JS edit until you regenerate. (A fixed date once left the owner looking at a stale stylesheet all afternoon.)

**Links off the site** open in a new tab with `rel="nofollow noopener"`: `external_links()` in
`gen_site.py` marks both pages on every run (the owner wants no search ranking passed to the pages
the site cites), so write a link plainly; a bare one fails `--check`.
See [[site-external-links-nofollow]].

**Faded is not hidden.** The story stacks a chapter's case figures in one grid cell and fades all
but the current one; at opacity 0 alone a later figure stayed on top and took every click meant for
the visible figure's link (owner, 2026-10-07: "those links don't work"). Faded figures are also
`visibility: hidden`, timed with the fade. Anything stacked and faded that holds a link needs the
same; the attack cases hold none, so they stay readable to screen readers.

**The social cards** (`tools/gen_seo_assets.py og`) are each landing page's own first screen,
rendered from a copy with every `<script>` stripped: the homepage's sandbox picture finished
(`og-image.png`), the overnight board at morning (`og-overnight.png`). Headless Chrome runs scripts even
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
- 2026-10-05 — Forks, Rotate and Integrations carry sourced figures; the Zed one is live (task 2026-10-05-add-sourced-figures-to-forks-rotation-and-integr).
- 2026-10-05 — Loops carries the owner's 120-task, 9-hour run (task 2026-10-05-add-the-owner-s-loops-figure-to-the-homepage).
- 2026-10-05 — small screens play the story as a deck with a camera on the picture; terminals replay on phones (task 2026-10-05-make-the-homepage-as-good-on-phones-as-on-deskto).
- 2026-10-05 — setup labels above their commands, the worker terminal gone, Loops says normal work (task 2026-10-05-homepage-polish-setup-labels-above-the-commands).
- 2026-10-06 — external links get target="_blank" and rel="nofollow noopener" from the generator (task 2026-10-06-external-links-on-the-website-open-in-a-new-tab).
- 2026-10-07 — faded case figures are hidden too, so they stop taking the visible figure's clicks (task 2026-10-07-homepage-citation-links-do-nothing-on-desktop).
- 2026-10-07 — the overnight page, its night board and the shared Overnight menu link (task 2026-10-07-second-landing-page-for-teams-that-keep-agents-w).
- 2026-10-07 — the overnight Project services row carries our measured 6.5 s (task
  2026-10-07-measure-the-warm-start-with-services-for-the-ove).
- 2026-10-07 — a copy change keeps the published commit count (task
  2026-10-07-overnight-page-tasks-come-from-any-acp-editor-no).
