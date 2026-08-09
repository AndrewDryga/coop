---
name: internal-import-dag
description: "a new internal import edge is an architecture decision — the allowlist test and this card move in the same commit"
scope: architecture
sources: [internal, internal/importdag_test.go]
check: "go test ./internal -run TestInternalImportDAG"
updated: 2026-08-09
---

# A new internal import edge is an architecture decision, not a convenience

The internal→internal import graph is frozen in the `allowedEdges` table in
`internal/importdag_test.go` — one line per package, listing every internal package it may import.
Adding an edge, dropping an edge, adding a package, or removing one means editing that table AND
this card in the SAME commit, with a changelog line here saying what moved and why.

Two invariants sit ABOVE the table and hold no matter how it is edited:

- **Nothing imports `internal/cli`.** cli is the top of the graph — it owns the flags, the wiring,
  and the terminal. An edge back into it inverts the architecture and makes every engine below
  depend on the CLI's shape.
- **`internal/ui` is imported only by the granted presentation owners** (`uiPresentationOwners`:
  cli, box, scaffold). Everything else returns data and lets its caller print it.

**Why:** the graph was already a clean DAG and nothing enforced it, so the next convenient import
would silently have become architecture — the way `internal/agent` had grown a `ui` dependency for
one badge color, and `internal/scaffold` an entire `box` dependency for one path. The 2026-08 review
plan pulls several engines out of `internal/cli` over the coming weeks; each extraction changes this
graph deliberately, and only a check that fails on the FIRST unplanned edge makes "deliberately"
mean anything. The repo's own rule is that a rule wants a `check:` that fails ([[gate-is-one-recipe]]);
this one has it.

**How to apply:**
- Need an edge that isn't in the table? Ask first whether the shared piece wants its own leaf
  package. If the edge is genuinely right, add it to `allowedEdges`, add a changelog line here, and
  keep the two in one commit. The test's failure message names both files.
- Deleting an edge is the same decision in reverse — the test fails when the table expects an edge
  the code no longer has, so a cleanup can't leave the table describing a graph that used to exist.
- Presentation stays at the edges: a library that wants `internal/ui` almost always wants to return
  data instead. Granting the edge costs two edits on purpose (the table AND
  `uiPresentationOwners`), and a grant that outlives its import fails too.
- The scan is stdlib-only (`go/parser`, ImportsOnly) and build-tag agnostic, so the frozen graph is
  the same under every GOOS and tag — `internal/runtime → internal/liveprocess` exists only behind
  `cooplivetest` and still counts. `_test.go` files are excluded (a test may import anything it
  needs — `internal/scaffold`'s tests drive `internal/box` on purpose), and so is `testdata`, whose
  fixture programs import internal packages to act as independent oracles ([[agents-are-one-file]]).

## Changelog
- 2026-08-09 — **+1 package, +2 edges: `internal/forkspace`** (`{"processidentity"}`) and `cli →
  forkspace`. The fork ON-DISK CONTRACT — workspace paths and names, the `<repo>-forks/.coop` state
  dir, the lifecycle flock, the pidfile wire format and its identity doctrine, clone/destroy/pin —
  came out of `internal/cli/fork.go` + `fork_loop.go` so the sessions service can share it by
  IMPORT instead of a 12-method injected interface (the prerequisite Plan A of the sessions
  extraction). Worker supervision stayed in cli, which is why forkspace takes NO `ui` edge:
  everything that printed (the reclaim warning, `fork ls`, the service teardown) is supervision and
  did not move. `git`'s hardening list moved with the clone that needs it, so cli's `gitArgs` now
  reads `forkspace.GitHardening` — one list in the tree, not two.
- 2026-08-09 — created, with the sweep that froze it: 20 internal packages, **40 edges**, 0
  violations — the allowlist IS today's graph, taken straight off the tree the commit before it
  (`1702d68`, which cut the last two edges that didn't belong: agent→ui and scaffold→box). Both
  directions and both invariants were proven against the real tree by temporary probes, not just
  asserted: a table edge the code lacks, a production import of `ui` from `config`, one of `cli`
  from `project`, and a brand-new package — each failed with the offending edge and both files to
  update.
