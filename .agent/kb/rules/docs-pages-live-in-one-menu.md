---
name: docs-pages-live-in-one-menu
description: every public docs page has one entry in the one menu, and published docs are linked on the site, never as a detour to GitHub
scope: docs
sources: [tools/gendocs/references.go, tools/gendocs/docsmap_test.go, tools/gendocs/search.go, site/docs.html, README.md, docs/README.md]
check: go test ./tools/gendocs -run 'TestEveryDocsPageIsInTheMenu|TestDocsDoNotSendReadersToGitHubForPublishedPages|TestReadmeSendsReadersToTheWebsite|TestOverviewSectionsLinkTheirDepthPage'
updated: 2026-11-04
---

# Every docs page is in the one menu, and the website is where docs are read

Add a docs page by adding its entry to `docsPages` in `tools/gendocs/references.go`. That entry puts
it in the menu on every docs page, in previous and next, and in search. Don't publish a page that
only other pages link to.

Readers read the docs on the website. If they need a document, publish it there and link it there.
Links to GitHub are for source code, examples and the project files in the menu's Project group
(changelog, contributing, issues). The README links website pages, not `docs/*.md`.

`docs.html` stays one guide that reads from start to finish; each section links the page that takes
its topic further.

**Why:** the owner, 2026-11-04: "Our docs nav is super complicated now - many pages not in navbar
then some docs are markdown in repo, links here and there bridging the two, hard to find anything".
Then, 17 of the 18 docs pages were in no menu. `evals.html` had one link pointing at it from
another page's text.

**How to apply:**
- New page: add the entry and the source together. A new Markdown file with no entry fails
  generation, and a new page with no entry fails the test.
- Before linking to GitHub from a page, ask whether a reader needs that document to use co:op. If
  so, publish it.
- Keep URLs and section ids. Released binaries print them, and `TestPublishedDeepLinksStillResolve`
  reads the CLI help's links to check them.

## Changelog
- 2026-11-04 — created with the shared menu. Swept every docs page and the README: `MIGRATING.md`
  and the README's verification steps were the only reader-facing documents left on GitHub, and
  both are published now. What remains links source files, `examples/evals/maintenance`,
  `testdata/protocol`, issues and releases.
