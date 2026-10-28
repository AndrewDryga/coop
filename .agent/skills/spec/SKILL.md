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

Produce a plan the user can approve, then hand to `/work`. Choose the simplest coherent
design that delivers the requested behavior. Read `AGENTS.md` and the code
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
Name the owners and files that must change to deliver the requested behavior. Walk the layers your
codebase actually has (schema / data / API / UI / tests) and skip the ones that
don't apply. For each piece, name the function/file and what it does.

- When changing domain state, define the data shape and invariants before the logic.
  Use the existing types and construction boundaries to prevent invalid states where practical;
  validate external data where it enters.
- For concurrent or persistent effects, name who owns mutable state and what happens on retry
  or a crash between effects. Prefer independent state where possible; make coordination
  explicit when sharing is necessary. Make retries safe through idempotence, deduplication
  or explicit recovery.

## 3. Choose boring on purpose
For each non-obvious decision, write the chosen approach and a one-line *why it's
the dull option*. If you reach for something clever (a new process, a macro, a
dependency), justify why boring can't do it — or drop it. An abstraction earns its place
by simplifying the current requirement or enforcing a concrete boundary; a possible future
caller is not enough.

Leave clear, working code alone when it already meets the requirement. Before adding nontrivial
defensive machinery, name a reachable workflow or credible attacker action, its consequence,
and why the benefit warrants the complexity. If an existing invariant rules a case out, identify
that invariant; if reachability is unknown, investigate it. Skip speculative extensions and
disproportionate fixes for harmless cases. Rare security or data-loss failures still matter.

When a new requirement invalidates an old assumption, compare a direct extension with the
design you would choose if that requirement had existed from the start. Include migration and
compatibility costs. Choose a focused redesign when it yields a simpler, correct result than
layering exceptions onto the old model. Deliver it in verifiable steps through the full approved
outcome; judge simplicity by the resulting system, not the size of the diff.

## 4. Output
- A numbered list of **small, independently verifiable steps**, each with its own
  done-condition (what gate proves it).
- The tests each step needs — happy path **and** the failure/denial path.
- Anything you're unsure of → a `/verify-api` to run, or a `decision.md` in the task folder
  entry if it needs a human call.

Ask for approval when the plan needs a new product/security choice or authority. Existing
approval covers its specified implementation; do not ask for it again. Do not build while the
user only asked for a plan.
