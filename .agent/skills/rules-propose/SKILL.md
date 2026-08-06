---
name: rules-propose
description: Mine this repo's own history for patterns that keep recurring, and propose them as rule cards for the human to accept or kill. Use when the rules KB feels thin, after a run of similar bugs, or periodically to catch taste nobody wrote down. Proposes; never writes a rule on its own.
argument-hint: "[--since <ref|date>] [subsystem]"
allowed-tools: Read, Grep, Glob, Bash, Write, Edit
---

# Propose rules from the repo's own history

Rule intake here is human-triggered: a correction becomes a rule. That misses the
mistake made three times that nobody happened to name. This skill reads the
evidence already on disk and proposes what it finds.

**You propose. The human accepts.** Never write a card without an explicit yes —
a rule is obeyed, so a wrong one does real damage.

## 1. Gather the corpus
Git history is the durable source. The task archive is NOT — `99_done/` gets
pruned and is usually near-empty; use it only if it happens to be populated.

```sh
git log --format='%h %s' --grep='fix\|Fix\|regress\|broke\|wrong\|stale\|revert'
git log --format='%h %s' --grep='^Revert'          # strongest signal: we shipped the wrong thing
git log --format='%h' --grep='fix' --name-only     # which files keep needing fixes
```

Scope it with `--since <ref|date>` when asked; default to the whole history the
first time and to "since the last proposal" after that. A named subsystem
argument narrows it to that path.

## 2. Cluster — two incidents minimum
One incident is a bug. A rule needs **≥2 independent** occurrences: different
commits, different days, ideally different files with the same failure shape.
Group by *what went wrong*, not by which file changed. Read the actual diffs of
the commits in a cluster before believing it — subject lines lie.

Discard clusters that are: a single incident, a one-off environment problem, or
already prevented by a test that now exists.

## 3. Dedupe against what's already written
`.agent/kb/rules/README.md`'s index is the routing table — one line per rule. Read
it and drop any cluster it already covers.

A cluster that IS already covered is the most valuable output this skill
produces: **the rule exists and is still being violated**, which means prose
isn't holding it. Report those separately as "graduate to a check", with the
recurring commits as the argument. That's the answer to "which rule should stop
being prose", backed by evidence instead of taste.

## 4. Report
For each surviving cluster, give the human:
- the rule as **one imperative sentence** (the card's `# title`)
- the evidence — the commit SHAs and what each got wrong
- proposed `scope:` and `sources:` (from where the fixes actually landed)
- a proposed `check:`, or `none` with a one-line reason nothing can gate it
- your call: propose, or "weak — here's what would make it a rule"

Rank by how often it recurred. Say plainly when you found nothing worth
proposing — a short honest list beats a padded one.

## 5. On accept — write it properly
Only after the human picks:
1. Write the card per the format in `.agent/kb/rules/README.md` (frontmatter +
   **Why:** citing the evidence commits + **How to apply:** + changelog).
2. **Sweep the tree for existing violations** — run the `check:`, or grep the
   `sources:`. Record what you found in the changelog. A rule that fires
   everywhere or nowhere is wrong: narrow it or drop it.
3. Add its index line to `.agent/kb/rules/README.md`.
4. `make rules-check`.
5. Fixing the violations you found is a SEPARATE task (`coop tasks add`) — the
   rule commit carries the rule, not a tree-wide refactor.
