---
name: spec
description: Design a boring-by-default implementation plan for a non-obvious or cross-owner change, or when asked to plan/design a feature. Routine fully specified work needs a short plan, not another approval round. Produces small verifiable steps.
argument-hint: "<what you want to build>"
allowed-tools: Read, Grep, Glob, Bash
---

# Plan a change (the boring, shippable way)

For routine fully specified work, write a short plan and proceed under the existing approval;
do not add an approval round merely because two files change. Use this fuller workflow for a
non-obvious approach or a consequential cross-owner/product/security decision.

Produce a plan the user can approve, then hand to `/work`. Optimize for the
smallest correct slice, not the grandest design. Read `AGENTS.md` and the code
you'll touch **before** planning — plan against the real code, not a guess.

## 1. Wear the PM hat first
Before *how*, settle *whether* and *what*:
- What's the job-to-be-done, and who for?
- What's the **smallest slice** that delivers it? Cut everything else into "later".
- That cut belongs before approval. Once the human has approved the whole feature, optimize the
  path, not the deliverable — never defer an approved part or weaken verification to finish
  sooner without them saying so.
- Does this already exist? If so it's a small edit, not a project.

If the request is vague or oversized, say so and propose the thin slice.

## 2. Map the work
Name what changes, and where, in the smallest set of files. Walk the layers your
codebase actually has (schema / data / API / UI / tests) and skip the ones that
don't apply. For each piece, name the function/file and what it does.

## 3. Choose boring on purpose
For each non-obvious decision, write the chosen approach and a one-line *why it's
the dull option*. If you reach for something clever (a new process, a macro, a
dependency), justify why boring can't do it — or drop it. No new abstraction that
serves one caller.

## 4. Output
- A numbered list of **small, independently verifiable steps**, each with its own
  done-condition (what gate proves it).
- The tests each step needs — happy path **and** the failure/denial path.
- Anything you're unsure of → a `/verify-api` to run, or a `decision.md` in the task folder
  entry if it needs a human call.

Ask for approval when the plan needs a new product/security choice or authority. Existing
approval covers its specified implementation; do not ask for it again. Do not build while the
user only asked for a plan.
