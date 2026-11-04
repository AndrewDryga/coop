---
name: docs-say-it-once
description: write for a capable reader; say each thing once in the sentence you would write to a colleague, with no over-explaining or twisted wording
scope: docs
sources: [README.md, docs, MIGRATING.md, CONTRIBUTING.md, site/README.md, tools/site]
check: none
updated: 2026-11-04
---

# Say each thing once, in the sentence you would write to a colleague

The reader is a capable developer. State the default, the command or the limit directly. Don't
define what they already know, explain the same thing twice, or give a reason they won't act on.
Don't twist a sentence to avoid a plain statement.

| Not this | This |
|---|---|
| If nobody chose a network mode for a box, the box is now filtered instead of open. | Boxes now use filtered networking unless you choose another mode. |
| An owner's reverse consumers are the packages that use its code. You choose the scope yourself: the Makefile does not select it from the files you changed. | Pick the scope yourself. The Makefile doesn't infer it from your changes. |

**Why:** on 2026-11-04 the owner rejected the first simple-English pass over the Markdown docs:
"this is not simple english, it's trickery wording, good text is something a human will read
easily, we don't need to over-explain, explain the same thing multiple times". That pass told
writers to keep every fact, put one idea in each sentence and explain internal terms. The docs came
out longer, choppier and talking down to the reader.

**How to apply:**
- Cut before you reword. If a sentence doesn't change what the reader does or decides, delete it.
- Say a fact in one place and link to it from elsewhere.
- Join fragments that belong together. Natural is the goal, and short sentences alone don't get
  there.
- A plainer doc usually comes out shorter. If a rewrite grew, it is probably over-explaining.
- Keep commands, limits, security promises and warnings about billing, deletion and data loss. An
  API reference keeps every field and rule.
- Match the owner's own writing: the opening of `README.md` and the approved sample in
  [[cli-text-is-plain-english]].

## Changelog
- 2026-11-04 — created. Swept the 14 Markdown files the first pass rewrote, plus the README; the
  second pass in task 2026-11-04-rewrite-the-public-markdown-docs-in-simple-engli rewrites them to
  this rule.
