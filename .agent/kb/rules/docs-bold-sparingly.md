---
name: docs-bold-sparingly
description: "bold marks structure, never mid-sentence emphasis, never inline code"
scope: docs
sources: [README.md]
check: "none"
updated: 2026-08-09
---

# Docs: bold is for structure, not emphasis

In the README and prose docs, bold (`**…**`) marks STRUCTURE — table-group headers, the
label column of a table (e.g. the troubleshooting left column), numbered-step labels, the
one hero tagline, and a short paragraph lead-in label (`**Term.**` opening a
definition-style paragraph). It is NOT for mid-sentence emphasis: don't bold a word or
phrase to stress it, and don't bold `inline code` (the monospace already sets it apart).

**Why:** scattered bold reads as shouting and stops being a signal — when a third of the
prose is bold, none of it stands out. Reserving bold for structure keeps it a scanning
aid. (The README had grown to ~100 bold spans, most of them inline emphasis.)

**How to apply:**
- Want to stress a point? Rephrase so the sentence carries it, or leave it plain; at most
  an occasional *italic*.
- A bold lead-in label that opens a paragraph as a sub-heading is fine; a bold phrase
  buried mid-sentence is not.
- Never bold inline code spans.
- Smell check: `grep -oE '\*\*[^*]+\*\*' README.md | wc -l` should be in the dozens, and
  nearly every hit should be a header, a table cell, a step, or a lead-in label — not
  prose emphasis.

## Changelog
- 2026-06-17 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: ran the card's own smell check
  (`grep -oE '\*\*[^*]+\*\*' README.md | wc -l` = 100 — one is a regex false-positive on a YAML
  glob `**` inside a fenced code block at README.md:1560, not real markdown bold); read full
  sentence context for every span whose bolded text wasn't obviously a header/numbered-step/
  lead-in-label/table-left-column (~25 candidates). 23 confirmed mid-sentence emphasis violations,
  e.g. README.md:481 "the **launched agent's** credentials", :546 "named **credentials**", :564
  "gives you **fallbacks**", :740/:743 "A **native** role"/"A **consult** role", :762 "must **not**
  commit", :1104/:1105 "gate **once**"/"signs off **again**", :1239 "their **own** queues", :1481
  "coop **validates it before every run**", :1639 "`--tasks` **replaces** this" (inside a table
  cell). Did not read context for the remaining ~75 spans (structural-looking on inspection:
  section headers, numbered steps, troubleshooting-table left column, `Term.`/`Term:` lead-ins) —
  a defined subset, not exhaustive. The count (100) and the fraction that's inline emphasis both
  land close to the card's own "Why" baseline ("~100 bold spans, most of them inline emphasis"),
  so the original cleanup looks incomplete or has regressed as prose was added since. Full list
  queued for the lead; not fixed here. The rule itself still looks sound — this is compliance
  drift, not a wrong rule.
- 2026-08-09 — fixed sweep: all 23 flagged spans de-bolded (re-located by quoted text — README
  churn earlier the same day had drifted line numbers, mostly +3 past README.md:~1460). 22 were
  plain markup removal (the word/phrase reads the same without it); :762 "must **not** commit" was
  reworded instead per the card's own guidance — "must never commit", so the prohibition still
  lands on a word that's hard to skim past, without the markup crutch. None were already resolved.
  Smell check now `grep -oE '\*\*[^*]+\*\*' README.md | wc -l` = 77 (100 − 23). `make align` green.
