---
name: review-board
description: The heavyweight pre-merge review — convene a board of expert hats (correctness, this repo's own rules, security, plus PM/UX/maintainer and any hat the change earns) as parallel reviewers, then synthesize ONE ranked verdict and an ordered plan to fix everything. Reads the project's OWN `.agent/rules/` and runs its OWN gate — no hardcoded laws. Read-only. Works on local changes, a commit, a range, a branch/ref, or a PR. Use before landing anything substantial, or when you want a thorough multi-perspective review.
argument-hint: "[nothing = local changes · commit · a..b range · branch/ref · PR number · -- pathspec]"
allowed-tools: Read, Grep, Glob, Bash, Agent
---

# /review-board — the panel reviews, you synthesize a fix plan

Convene a board of expert hats; each reviews the change through ONE lens; then YOU
(the parent) synthesize one ranked verdict and an ordered plan to fix everything.
The hats read this repo's **own** taste — `AGENTS.md` plus every `.agent/rules/*.md`
— not any fixed checklist, so the review sharpens as the project's rules grow.
**Read-only: the board reviews; it never edits.** The deliverable is a *fix plan*, not just notes.

## 1. Scope the change
Resolve the argument into a concrete diff + file list, then read it yourself first:
- **nothing (default)** → uncommitted local changes: `git diff HEAD` plus untracked files from `git status --porcelain`.
- **a commit** (`a1b2c3d`) → `git show <hash>`.
- **a range** (`a..b`, `HEAD~3..HEAD`) → `git diff <range>`.
- **a branch / ref / tag** → its diff vs the base: `git diff main...<ref>`.
- **a PR number** (`123`) → `gh pr view <n> --json title,body,files` + `gh pr diff <n>` (read title/body for **intent**).
- **a pathspec** (`-- internal/…`) → narrows any of the above.

**Shared checkout:** with a concurrent agent editing, `git diff HEAD` shows *everyone's* work, not just yours — narrow with a pathspec, or review a specific commit/PR, when you want only your slice. For intent on a non-PR scope, read the commit message(s) in range, or — for uncommitted work — the in-progress task's `task.md`/`log.md` under `.agent/tasks/10_in_progress/`. If what the change is *for* is unclear, say so.

**Announce the resolved scope** (input mode + exact file list) and note what it touches — logic, user-facing surface, docs, config — which drives the hats.

## 2. Run the gate first
Run the repo's gate (`AGENTS.md` → "The gate"). A **red gate is an automatic BLOCKER** — reviewing code that doesn't build or pass is moot. If you can't run it (a historical commit/PR you can't build locally), record `gate: unverified` rather than guessing.

## 3. Convene the hats the change earns
**Standing — always, for any code change:**
- **Staff engineer** — correctness & edge cases, the simplest thing that works, over-engineering, maintainability, "would I approve this PR?"
- **Rules** — does it obey `AGENTS.md` (the creed) and **every** `.agent/rules/*.md`? Read them; check the diff against each. (No rules yet → just the creed; don't invent any.)
- **Security** — lead with the abuse case: untrusted input, secrets, authz, injection, data leak.

**Earned — add when the diff touches the surface (lean toward MORE coverage):**

| The diff touches… | Add |
|---|---|
| user-facing output, CLI flags, prompts, errors, or UI | **UX** |
| a new capability, command, flag, or flow | **PM** — the right thing? the smallest slice? |
| tricky control flow meant to live for years | **Maintainer** — clear in six months? |
| a surface the project ships its own hat-skill for | that hat — load its `.agent/skills/<name>/SKILL.md` as the checklist |

## 4. Fan out — one reviewer per hat
If your runtime has parallel subagents (e.g. an Agent/Task tool), spawn one per hat in a single batch; if not, walk the hats yourself, one at a time. Either way give each the ref + touched files and this brief:

> You are the **<hat>** reviewing `<ref>`. If `.agent/skills/<hat>/SKILL.md` exists, load it as your checklist; else use the <hat> lens. Read the touched files **in full** — plus their callers/tests, not just the diff hunks. Review **only** through your lens. Report each finding as `SEVERITY · path:line · issue · why it matters · fix`. SEVERITY ∈ BLOCKER / MAJOR / MINOR / NIT. Be specific to THIS change; skip what's clean; don't invent findings to look thorough. **Do NOT edit — review only.** ≤300 words.

Tell the security + rules hats to name the exact abuse case / the exact rule each finding breaks.

## 5. Synthesize (you, the parent — don't just concatenate)
- **Dedupe** overlaps (UX + a UI hat flag the same thing — merge, keep the sharpest wording).
- **Rank** BLOCKER → MAJOR → MINOR → NIT. A **BLOCKER** ships the wrong thing, a security hole, data loss/leak, a documented-rule violation, a red gate, or a real correctness bug.
- **Resolve cross-hat conflicts explicitly** — PM wants it thin, security wants a gate: state the call + why.
- One-line **verdict**: SHIP / SHIP-AFTER-BLOCKERS / RETHINK.

## 6. Write the fix plan (the deliverable)
```
## Review board: <title>   —   verdict: <…>   (<N blockers, M major, K minor>)
gate: <green | red: … | unverified>

Headline: <2–3 sentences — ship-ability + the one thing that matters most>

### Findings (ranked, deduped)
- BLOCKER [security · staff] path/to/file:42 — <issue> → fix: <…>
- MAJOR  [ux] …

### Fix plan (ordered; blockers/riskiest first)
1. <fix> — <what + where> — closes: [security:1, staff:3] — <independent | needs #N>
2. …

### Hat notes (one line each)
staff: … · rules: … · security: … · pm: … · ux: …
```

Keep it honest and short. If it's clean: **"SHIP — nothing blocking"** + the few suggestions worth the reader's time. Never manufacture findings to look thorough; never bury a real blocker in nits.

**Then offer to queue it:** turn the BLOCKER/MAJOR items into tasks — `coop tasks add "<title>"` (one per fix, lands in `00_todo/`) so `/sweep` can drain them; smaller notes go in `.agent/BACKLOG.md`. Append on the user's go — never silently.

## Relationship to other reviews
`/review-board` is the heavyweight, on-demand whole-change review that ends in a plan. `/sweep` already runs a lighter self-review per item in its loop; reach for `/review-board` before landing anything substantial. Where your runtime has single-lens reviewers (a bug-only or security-only pass), this convenes those lenses and more — plus the fix plan.
