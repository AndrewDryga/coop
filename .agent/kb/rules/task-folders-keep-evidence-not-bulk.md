---
name: task-folders-keep-evidence-not-bulk
description: "a task folder keeps evidence, never rebuildable bulk (record commit, sha256 and build command); scratch goes in tmp/; only the layout sits at the queue root"
scope: agent-workflow
sources: [internal/tasks/clutter.go, internal/tasks/cmd.go, internal/tasks/queue.go, internal/loop/loop.go, internal/loop/prompts.go, internal/taskmcp/tools.go, internal/scaffold/templates/agent/tasks/README.md, .agent/tasks/README.md]
check: "go test ./internal/tasks -run 'TestDoneWarnsAboutRebuildableBulk|TestTaskBulkWarningSkipsScratch|TestTaskBulkWarningStopsAtItsLimits|TestListWarnsAboutQueueRootStrays'"
updated: 2026-10-16
---

# Keep evidence in a task folder, never what a build can recreate

A task folder is the archive a reviewer or future maintainer reads. `artifacts/` holds evidence:
repros, captured output, the logs that prove a check ran. It does not hold rebuildable bulk:
compiled binaries, release tarballs, toolchains, virtual environments, Git packs. For those,
record the commit, the sha256 and the exact build command in `log.md`, and keep the file
somewhere disposable. Scratch work goes in `tmp/`, which done removes. Only `README.md`, the four
state folders and `xx_backlog/` belong at the queue root; release notes and logs go in their
task's folder or outside `.agent/tasks`.

Coop warns and never deletes; what to keep is the owner's call:
- Completing a task warns, without refusing, when its folder holds a compiled program (an
  executable ELF or Mach-O file; a script is readable evidence) or more than 25 MB: `coop tasks
  done` and `coop loop` under its completion line, and the in-box `tasks_complete` result for the
  agent. The agent's version asks it to leave the folder and say what the files are in its final reply,
  since it was just told to write nothing more there. The check measures the whole folder except
  its top-level `tmp/`, which done removes, since agents also leave builds at its top level and in folders of
  their own naming. Past 64 folder levels or 10,000 entries it stops and says the folder could
  not be measured fully: a box can build such a tree, and the in-box tool measures while the box
  waits.
- `coop tasks` and `coop tasks ls` end with a warning about any other file or folder at the queue
  root. Hidden entries never count: Coop keeps `.coop-queue.json` there, and tools keep their own
  (`.conflux/`, `.DS_Store`).

**Why:** the 2026-10-15 maintenance audit (GC-07) found this repo's `.agent/tasks` at 2.4 GB: 38
compiled binaries made up about 0.9 GB (about 600 MB of it in `99_done/`, growing about 120 MB a
week since September), one backlog item's `tmp/` held 675 MB of toolchain tarballs and a Python
virtual environment, and 170 MB of release files sat at the queue root. No code wrote any of it;
agents did, and `coop tasks done` only ever removed `tmp/`.

**How to apply:**
- Before `coop tasks done`, look at what the folder keeps. A binary built for a check is not
  evidence; its commit, sha256 and build command are.
- Large evidence that cannot be rebuilt (a captured trace, a set of screenshots) may stay. The
  warning asks for a decision; it is not a verdict.
- Never quiet a warning with automatic deletion ([[fix-the-bug-not-the-feature]]). Pruning stays
  `coop tasks rm` and a person's call.

## Changelog
- 2026-10-16 — the loop prompt and the `tasks_complete` description now state the rule before completion
  (programs and rebuildable bulk go in tmp/; a binary is recorded by commit, sha256 and build command).
- 2026-10-16 — the loop's finalize step and the in-box `tasks_complete` result warn too: they are
  how unattended agents finish tasks, so done alone missed most bulk. `tmp/` stopped counting,
  because assigned in-box completion measures before the host removes it: coop's queue has four
  top-level `tmp/` entries (1.2 MB to 692 MB, the largest in two backlog items) that would
  otherwise have read as archived bulk. One of them sits in a `99_done/` folder, left for the
  owner. The walk opens each folder from its parent and stops past 64 levels or 10,000 entries:
  through `os.Root`'s whole-path lookups a chain of 500 folders took 5 s and 2,000 folders 64 deep
  took 4.5 s, now milliseconds. Coop's fullest finished task holds about 1,400 entries and its
  deepest folder is 10 levels down, so evidence stays well inside both limits.
- 2026-10-15 — created from the audit's GC-07. Swept every lifecycle task folder in coop (832)
  and emisar (161): done would warn on 17 and 5 of them (14 and 1 hold compiled programs; 16 and
  5 hold more than 25 MB). Those 22 pre-existing folders are left for the owner to review; never
  auto-deleted. In coop's `artifacts/` folders an executable-bit test alone would have
  named 32 scripts beside the 20 real programs, so the check reads the file header. Measuring only
  `artifacts/` would have missed 3 programs (103 MB) at coop task roots and 4 of emisar's 5 large
  folders (bulk in `probes/`, `reviews/`, `published-*/` and the task root). Both queue roots
  hold only their layout and hidden entries today (the audit's strays were removed since): 0
  warnings.
