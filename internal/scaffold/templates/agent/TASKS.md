# .agent/TASKS.md — the work queue.
# [ ] todo   [w] claimed/in-progress   [x] done+gated+committed   [B] blocked
#
# Tasks are self-contained: a fresh agent with only the BOOT files (this queue,
# AGENTS.md, LOG.md) and the repo must be able to do each one — no prior chat, no
# remembered review. Give every task the five-part shape in the ## Example below.
# EVERY top-level `- [ ]` here is live work, wherever it sits — `## Active` is just a
# convention, not a fence. The example uses [E] so it's skipped; `#` comments are too.
# Anything not ready to work belongs in BACKLOG.md / IDEAS.md, never as a `- [ ]`.

## Example

- [E] <one-line outcome — the smallest right thing>
  - **Context:** the problem, why it matters, and where it lives in the code.
  - **Likely files:** the files you expect to touch.
  - **Implementation direction:** the boring approach; constraints; what to avoid.
  - **Acceptance checks:** how to know it's done — the gate green, plus the behavior or test that proves it.

## Active
