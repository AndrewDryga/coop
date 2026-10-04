---
name: secret-shadow-walk
description: every launch walks the whole checkout, gitignored task folders included, to hide secrets; its cost grows with local clutter, so the decider remembers directory verdicts and lstats for .coopignore
subsystem: box
sources: [internal/box/mounts.go, internal/shadowpath/shadowpath.go, internal/shadowpath/shadowpath_test.go, internal/box/run.go, tools/lifecycle_bench.py]
updated: 2026-10-04
---

`box.ComputeMounts` runs on every launch (`run.go`, and `restricted.go` for read-only and bare
runs). It walks the whole repository, skipping only `.git` and directories it hides, and asks
`shadowpath.NewDecider` about each path. Everything in the checkout is visible in the box,
gitignored working state included, so `.agent/tasks` (artifacts, `tmp/` worktrees and clones) is
walked too and can't be skipped. A secret in a task's scratch clone is still a secret.

**Cost follows local clutter, not the repo.** On 2026-10-04 this checkout held 44k files in 12k
directories, 41k of them under `.agent/tasks`. The walk took 4.1 s, while a clean clone of the same
commit took 0.05 s, and a filtered warm start took 8.1 s against the clone's 3.1 s. Two costs
multiplied:

- `shadowed` re-decided every ancestor of every path: 64 secret globs plus the `.coopignore`
  chain at each level.
- `loadDir` opened the repository root and every component, looking for a `.coopignore` that
  almost no directory has.

`NewDecider` now remembers each directory's verdict (a path costs its own check plus one lookup)
and lstats before the safe read (only "not there" skips it). `shadowed` stays the plain definition
that the `Snapshot` uses, and `TestDeciderMatchesShadowed` pins the two together, including links
the safe read refuses. What remains is the walk's own directory reads (about 0.4 s for 12k dirs).

**Measuring start time:** `make lifecycle-bench` on a clean clone gives the product's number. A
cluttered checkout measures its clutter.

## Changelog
- 2026-10-04 — created with the decider memo and lstat precheck (task 2026-10-04-find-why-a-warm-start-takes-8-s-in-a-checkout-wi).
