---
name: cli-text-is-plain-english
description: CLI docs, website copy and messages use direct English while preserving commands, facts and safety details
scope: cli-output
sources: [internal/cli/help.go, internal/cli/models.go, internal/cli/usage.go, internal/box/launch_sections.go, internal/tasks/cmd.go, docs, install.sh, tools/site, tools/gendocs/references.go, site/docs.html]
check: none
updated: 2026-10-21
---

# Write public docs and messages in plain English

Say what happened and what the user can do next. Prefer "could not check this sign-in" to
"authentication authority unavailable", and "saved private commits" to "committed custody".
Keep technical names when users need them to find a file, understand a restriction or use an API.

Use ordinary sentence punctuation. Cut staged introductions, unsupported guesses, inflated
claims and sentences that repeat the previous point. Keep useful headings, status symbols and
tables. A warning about missing evidence, billing or deletion carries information; do not remove
it merely because it contains a contrast or qualification.

Describe the observed state without adding a cause, time or stage the code does not establish.
Do not replace every dash mechanically; a separator can be clearer than repeated colons.
Keep edited Markdown paragraphs wrapped without changing their examples or technical meaning.

Preserve command syntax, flags, paths, configuration/API values and link targets. Never rewrite
provider output or protocol values. Trace callers before changing a string used for comparisons,
stored templates or both human and machine output. Generated docs and transcript fixtures must
follow their source. A changed expected sentence is not permission to weaken its behavior test.

Apply the same review to website introductions, walkthroughs and reference summaries. Use
Humanizer when a passage sounds staged or repetitive. Use the user-approved passage below as
the voice sample, rather than copying the existing technical prose. Split dense paragraphs and
replace vague nouns with the action the reader takes. Keep
useful technical terms, real safety distinctions and stable heading anchors. Edit generated
pages through their source, then compare examples, links and anchors with the previous output.

Review whole sections, including lists and tables. Fixing only the most conspicuous jargon is
not a complete polish pass. Explain what co:op does, what the reader needs to do, and what
happens if a step fails. Replace internal nouns such as "qualification", "custody" and
"admission" with those actions unless the reader needs the exact term to use an API. Keep
protocol fields and commands literal, and explain them in ordinary English nearby.

Make conditions easy to follow. Start with what the reader needs to do, put independent
requirements in a short list, then explain what happens if they are not met. Explain internal
terms when they matter: “workload identity” becomes “a saved ID for the tasks and how they run”.
Use a small example for unfamiliar measurements, such as percentage-point changes. Preserve
the conditions, but do not make the reader decode a sentence shaped like a validation function.

**Approved voice sample:**
> Before your first filtered session, co:op runs five checks to make sure Docker can enforce the
> network rules. It shows you the results, then starts your session if all five pass. If Docker
> isn't running or a check fails, co:op explains the problem and won't start the session.
>
> Later runs skip these checks while the saved results are still valid. You still need to approve
> what each project can access on the network.

**Why:** the user asked to apply [Humanizer](https://github.com/blader/humanizer) to all CLI docs
and outputs and to use simple English where it helps. This pass used version 3.1.0 at commit
225a6f39ac85f76ee48dbad772ea4abe4ed6c9d8. Its factual-preservation and technical-voice rules
control the edit; editorial judgment is still needed, so there is no keyword-ban check.

## Changelog
- 2026-10-21 — the user flagged the evaluation comparison explanation as too complex after
  review. Replaced stacked conditions with reader actions, explained the saved workload ID,
  separated grading requirements and added a percentage-point example. Swept the evaluation
  reference, configuration, usage reports and public evaluation summary for the same pattern.
  Simpler vocabulary alone is insufficient; review sentence structure and reading order too.
- 2026-10-21 — the user rejected the selective pass as unnatural and approved the example above.
  Rewrote complete sections across authored references, walkthroughs and the website overview;
  reviewed the generated CLI without changing its commands. Corrected misleading credential,
  fork and warm-session descriptions against their implementations. Kept before/after evidence
  under the task's reader-english artifacts. Isolated worktrees hold advisor proposals; the lead
  reviews and applies them to the shared checkout.
- 2026-10-21 — the user extended the plain-English and Humanizer review to new website content.
  Reviewed both walkthroughs, all 11 public references, summaries and new entry links. Simplified
  dense prose at its source; retained the already-reviewed generated CLI text. Source checks also
  corrected stale peer-credential mount descriptions. Retained before/after and preservation
  evidence under the search-visibility task's plain-english artifacts.
- 2026-10-13 — the user approved three self-review corrections: describe current rate limits
  without attributing earlier task limits to preflight, restore preset-mode separators and
  rewrap the isolated-fork recovery paragraph. Swept preset-mode source/tests/fixtures and
  nearby loop limit notices; only the all-target notice incorrectly attributed earlier limits.
- 2026-10-12 — surveyed all help pages, 13 public prose guides and 208 runtime Go sources plus
  the installer. Simplified dense help, usage, service, network, task and fork wording. Removed
  a retired coop ps remedy and unsafe duplicate-task deletion advice. Preserved decision scaffold
  markers, cancellation/staleness comparison strings, shared JSON verdicts and provider bytes.
  Changes remain local and pending the user's review; no publication or prior approval is claimed.
