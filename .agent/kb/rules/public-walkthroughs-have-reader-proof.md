---
name: public-walkthroughs-have-reader-proof
description: public walkthroughs need a coherent worked path, observable checkpoints and adversarial reader review
scope: docs
sources: [tools/site/sandbox-guide.tpl.html, tools/site/overnight-guide.tpl.html, site/README.md, brand/DESIGN.md]
check: none
updated: 2026-11-04
---

# Public walkthroughs need reader proof

A public walkthrough must help a new reader complete and verify one coherent workflow.
Before publication, review it adversarially for source accuracy and the reader's experience,
then inspect the rendered desktop and mobile pages. Passing generation checks alone does
not establish publication quality.

- State the purpose briefly and list prerequisites before the first action. Use one reading
  sequence; do not repeat the introduction as an outcome sidebar and workflow strip
  alongside the actual contents. Start the desktop contents beside the introduction, on the same
  top line, so they do not hang below an empty column. Keep intro text at a comfortable measure. Label where commands run.
- Provider-guide prerequisites are step00 in the article and contents, immediately above
  “What this sandbox controls”; keep them out of the hero.
- Open sandbox guides with a short, scannable list: limited file access, shadowed secrets,
  restricted networking and provider credentials kept outside the container. Put native-client
  permission flags and implementation detail beside the relevant step or in a disclosure.
- Review cognitive load and reading order, not just overflow and component legibility. The
  reader should find one obvious route from title to preparation to the first action.
- Group provider walkthrough contents into setup, feature demonstrations and troubleshooting.
  Keep each feature’s related steps together, with matching article order and numbering.
  When moving a step, preserve its host/agent context and any shell state used later.
- Carry one worked path through preparation, execution, recovery and review. Never switch
  silently from a fork to the parent checkout or lose queue scope on resume.
- Show the results of consequential steps and explain failures inline. Do not add repeated
  “What to look for” or “Check before continuing” callouts that restate the instructions.
  Assume a capable reader who needs domain facts, not reminders to follow each step.
- Apply reader feedback across the whole walkthrough, paragraph by paragraph, including
  its provider variants. Explain what a step demonstrates; omit obvious directions such as
  “copy and run the whole block” and repeated reminders to inspect output. Put optional
  setup alternatives and implementation details in named help disclosures. Preserve approved
  wording and the conditions that make a check meaningful.
- Execute published walkthrough commands where possible and retain a command-by-command
  ledger. Source review, syntax checks and fixtures are separate evidence. Add short output
  excerpts at consequential steps; label variable samples and expected results, preserve
  unexecuted/login-blocked rows, and never present a fixture as a real provider session.
- Replay the published blocks in a real terminal with the real agent before calling a guide
  verified. A non-terminal run skips interactive prompts, and a raw container misses the agent's
  own permission rules and the environment its shell tool really gets. Take sample output from
  those sessions, since a raw container's run list looks nothing like an agent's.
- Do not add terminal-location captions such as “Host terminal” or “Inside Claude Code ·
  shell tool” above provider-guide commands. Let the surrounding steps establish where to
  run them; explain transitions once in ordinary prose.
- Give a safe sequence of related commands one copy control, rather than one per line.
  Use `&&` between dependent setup steps, so a failure stops the block. Keep human approval
  separate. For run IDs, show a realistic listing, identify the session, and tell the reader
  to replace the example ID in the next command. Do not add shell `read` prompts or temporary
  variables where a direct CLI example is clearer.
- Show a command and its output in one terminal block, with a blank line between them.
  Make output slightly dimmer than the command while keeping it easy to read. Keep output
  closer to its command than to the next command, so each pair reads as one group.
  Remove per-output captions such as “Example version · yours may be newer”. Keep the copy
  source limited to the command; preserve useful host/agent and platform context. Put each
  output immediately after the command that produced it, not after an unrelated command.
- Keep disposable approval-demo projects on the same filesystem as co:op’s state. Use a
  temporary directory under the home directory for the normal setup; `/tmp` can be a separate
  filesystem on Linux. Verify the approval itself, not just the earlier blocked request.
- A sandbox walkthrough demonstrates its boundaries in separate worked sections: a harmless
  secret is hidden, an actual request is blocked, one website is approved and retried, the
  correct run’s network receipt is inspected, and the provider’s public credential stand-in
  is checked without printing secrets. Run checks inside the provider session; a separate raw
  shell has different credentials and cannot prove the provider container’s view.
- Distinguish an illustrative task from actual command output or a measured result.
- Keep critical boundaries beside the affected action; put only optional explanations and
  alternatives in named disclosures. Required steps, including credential-check scripts,
  stay visible in the main reading path. Preserve the full reading path without JavaScript.
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

- 2026-11-02 — owner required hands-on secret, network, receipt and credential demonstrations
  in the provider walkthroughs. Replaced summary-only steps across all four guides with a
  disposable project and explicit observations. Empty shadow files and denied eBPF reads are
  both valid; failed requests require a recorded block, and absent evidence stays unverified.

- 2026-11-02 — owner found the opening four paragraphs too complex. Replaced them across
  the four provider guides with one sentence and four concrete boundaries. Moved native
  permission-mode explanations to an expandable note at launch.

- 2026-11-02 — owner requested executed-command coverage and expected outputs. Added
  important file, credential, network, approval and receipt output examples; started a fresh
  command ledger and corrected the new guides’ installer URL to the currently working host.

- 2026-11-02 — owner moved provider prerequisites into the numbered walkthrough as step00,
  directly before the boundary summary. Updated all four guides and their contents links.

- 2026-11-02 — Linux Cloud Shell replay exposed cross-device approval failure from /tmp.
  Moved all four demo folders under $HOME, retained the failure, and added observed Linux
  zero-byte shadow output alongside the protected-Mac example.

- 2026-11-02 — owner requested command/output as one terminal block without example-version
  captions. Combined all11 output examples per provider guide, kept command-only copy controls
  and required platform/context labels, and documented the shared terminal pattern.

- 2026-11-02 — owner requested copying safe command sequences as a block. Grouped demo setup,
  dummy-file creation, guarded agent launch and Git review on all4 provider guides. Used fail-stop
  chains; kept approval and run-ID entry commands separate. Actual host replay and injected
  failure checks distinguish shell-flow proof from provider/model execution.

- 2026-11-02 — owner rejected repeated “What to look for” callouts as patronizing. Removed
  provider-guide checkpoint frames and redundant progress summaries; kept unique failure
  conditions, credential scope and recovery as ordinary prose beside the relevant command.

- 2026-11-02 — owner grouped the contents by setup, protection feature and troubleshooting.
  Swept all four provider variants; grouped their static contents, moved network inspection
  beside its demonstrations and made the second-terminal/original-shell transitions explicit.
  The two general guides have different workflows and remain unchanged.

- 2026-11-04 — owner approved the real signed-in replay of all four provider guides on macOS.
  Found three things no earlier check could: `coop init` asks two questions the guides never
  mentioned, so the next pasted block goes into them; Claude Code refuses the secret check through
  the `.env` deny rule `coop init` writes (and Grok follows it when that file exists); Claude Code
  hides its token variable from its shell tool, so the published credential script could only fail.
  Swept all four guides, fixed these, and replaced raw-container samples with real session output.
  Added the `coop init` note to the general sandbox guide too; the overnight guide does not run it.
  Samples are short excerpts where a real value varies by reader, such as the setup-file count.
