---
name: secret-shadow-walk
description: every launch walks the whole checkout, gitignored task folders included, to hide secrets; its cost grows with local clutter, so the decider remembers directory verdicts, lstats for .coopignore and pre-filters its globs
subsystem: box
sources: [internal/box/mounts.go, internal/shadowpath/shadowpath.go, internal/shadowpath/shadowpath_test.go, internal/box/run.go, tools/lifecycle_bench.py]
updated: 2026-10-06
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
the safe read refuses.

**The rest was CPU, not disk** (measured 2026-10-06, 40k paths in 6k directories, warm). The walk's
own directory reads took 0.12 s; deciding took 0.47 s, of which `filepath.Match` against the 64
secret globs was 0.31 s (7.8 µs a name) and re-walking every ancestor's `.coopignore` level was
most of the rest. The decider now matches through a `globSet` (a pattern's literal start and end
rule out almost every name before `filepath.Match`, which still decides the rest;
`TestGlobSetMatchesFilepathMatch` pins it) and consults only the ancestors whose `.coopignore` has
rules. A pass over the 40k paths went from 518 ms to 67 ms, alternated under the same load, with
the same answer for every path. Don't cache directory listings between launches to win the last
0.1 s: the cache would be a stored file the box must never reach, guarding the code that hides
secrets. Profile this walk with direct timing; the macOS CPU profiler pins most of it on syscalls.

**Measuring start time:** `make lifecycle-bench` on a clean clone gives the product's number. A
cluttered checkout measures its clutter. After the globSet change (2026-10-06, before and after
builds alternated, load 8 to 10), a filtered start in this checkout took 2.62 s against 3.18 s,
and a clean clone 2.30 s; on 2026-10-04 the same checkout took 8.1 s.

## Changelog
- 2026-10-04 — created with the decider memo and lstat precheck (task 2026-10-04-find-why-a-warm-start-takes-8-s-in-a-checkout-wi).
- 2026-10-06 — the remaining cost was glob matching, not directory reads: globSet and policy chains (same task).
