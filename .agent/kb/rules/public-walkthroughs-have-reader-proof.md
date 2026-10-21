---
name: public-walkthroughs-have-reader-proof
description: public walkthroughs need a coherent worked path, observable checkpoints and adversarial reader review
scope: docs
sources: [tools/site/sandbox-guide.tpl.html, tools/site/overnight-guide.tpl.html, site/README.md, brand/DESIGN.md]
check: none
updated: 2026-10-20
---

# Public walkthroughs need reader proof

A public walkthrough must help a new reader complete and verify one coherent workflow.
Before publication, review it adversarially for source accuracy and the reader's experience,
then inspect the rendered desktop and mobile pages. Passing generation checks alone does
not establish publication quality.

- State the purpose briefly and list prerequisites before the first action. Use one reading
  sequence; do not repeat the introduction as an outcome sidebar and workflow strip
  alongside the actual contents. Anchor the desktop contents and article under a page-wide,
  left-aligned introduction, so the contents do not hang below an empty column. Keep intro
  text at a comfortable measure. Label where commands run.
- Review cognitive load and reading order, not just overflow and component legibility. The
  reader should find one obvious route from title to preparation to the first action.
- Carry one worked path through preparation, execution, recovery and review. Never switch
  silently from a fork to the parent checkout or lose queue scope on resume.
- Give consequential steps an observable checkpoint and a useful next action when blocked.
- Distinguish an illustrative task from actual command output or a measured result.
- Keep critical boundaries beside the affected action; put optional alternatives in named
  disclosures. Preserve the full reading path without JavaScript.
- Check real copy behavior, keyboard access, narrow-screen layout and failure feedback.
  Retain findings and their disposition; source review is not live-provider verification.

## Changelog
- 2026-10-20 — user requested substantially better public walkthroughs and adversarial review
  before publication. Swept both new guide templates and the existing reference docs. The
  guides needed a worked task, explicit stop/resume, consistent fork and queue scope, early
  gate setup and better visual hierarchy; the reference docs remain the detailed command
  owner. Applied the walkthrough pass to both guides and retained its review/browser evidence
  under the search-visibility task. Editorial quality remains a review check.

- 2026-10-20 — user found the guide tops dense and scattered despite the earlier visual pass.
  Removed both outcome sidebars and redundant workflow strips, shortened the leads, and
  aligned ordinary prerequisites with the article. Review now explicitly checks competing
  summaries and reading order.

- 2026-10-20 — user found the contents dangling under the empty left half of the header.
  Moved both introductions to the page’s left edge and gave the header a full-width divider
  above the contents/article layout. The simple reading order and short copy remain.
