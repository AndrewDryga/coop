---
name: spec
description: Design an opinionated, boring-by-default implementation plan before writing code. Use when a change spans more than one file or context, when the approach isn't obvious, or when asked to plan/design/think through a feature. Produces a small-step, verifiable plan to approve before /work.
argument-hint: "<what you want to build>"
allowed-tools: Read, Grep, Glob, Bash
---

# Plan a change (the boring, shippable way)

Produce a plan the user can approve, then hand to `/work`. Optimize for the
smallest correct slice, not the grandest design. Read `AGENTS.md` and the code
you'll touch **before** planning — plan against the real code, not a guess.

## 1. Wear the PM hat first
Before *how*, settle *whether* and *what*:
- What's the job-to-be-done, and who for?
- What's the **smallest slice** that delivers it? Cut everything else into "later".
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

Stop and let the user approve before `/work`. Don't start building from `/spec`.
