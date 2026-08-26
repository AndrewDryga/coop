---
name: internal-import-dag
description: "a new internal import edge is an architecture decision — the allowlist test and this card move in the same commit"
scope: architecture
sources: [internal, internal/importdag_test.go]
check: "go test ./internal -run TestInternalImportDAG"
updated: 2026-08-25
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
  box, cli, forkctl, loop, scaffold, tasks). Everything else returns data and lets its caller print it.

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
- 2026-08-25 — **no graph change:** removed Fleet's parser, lifecycle, status board, and launch
  wiring from `internal/forkctl`/`internal/cli`. `forkctl` still needs every frozen edge for direct
  fork supervision, listing, review, removal, and merge, including its presentation-owner grant;
  `TestInternalImportDAG` remains green unchanged. Corrected the allowlist comment so only `tasks`
  is described as owning a live board.
- 2026-08-25 — **package rename, edge rename, −1 cli edge:** `internal/fusion` became
  `internal/consult` after the mandatory Fusion command/state was removed. `box → fusion` became
  `box → consult`; the package keeps its single `consult → agent` edge; `cli → fusion` disappeared
  because the CLI now reaches consultation only through `box.RunSpec`. `acpctl → box` also
  disappeared with its council resolver. No new edge was introduced.
- 2026-08-10 — **+1 package, +1 edge, +1 `uiPresentationOwners` grant: `internal/loop`**
  (`{"agent", "box", "config", "forkspace", "ladder", "loopcfg", "preset", "runtime", "tasks", "ui"}`);
  `cli` GAINED `loop` and **dropped nothing** — every edge on cli's line survives, `loopcfg` and
  `ladder` specifically because the COMMAND stayed. The LOOP ENGINE, last and largest of the 2026-08
  extractions: ~7.1k non-test lines (47% of what `internal/cli` had), the engine body plus its organs
  (stream-JSON decoders, provider watchdog, rate-limit caps, telemetry, change detection, the live
  bar). Its shape was neither of the two the earlier extractions had: the loop is a **SINK** — 18
  outbound identifiers against 6 inbound, and only **7** production call sites reach into it — so
  nothing in cli is built ON it, which is the cleanest possible seam and made the whole question the
  outbound side. Three `app` fields (`runID`, `streamSeq`, `streamOff`) turned out to be loop-only
  and left with it, plus `forkOwner`, which became a `RunSpec` field the caller sets instead of a
  mutate-and-restore on the shared struct: **`app` went from 11 fields to 7.** The cut line is the
  same one `forkctl` drew, one level up — `parseLoopArgs`/`cmdLoop`/`resolveWorkAgent` stay as
  `cli/loop_cmd.go` (cli owns the COMMAND: argv → preset, target, peers, queues, rotation, image;
  loop owns the RUN), which is what keeps the Host at **three** fields (`SweepOrphanBoxes`,
  `SignUnpushed`, `BuildRotation`) instead of eleven, AND what keeps cli's `loopcfg`/`ladder` edges.
  The running coop's VERSION is `-ldflags`-pinned to `internal/cli`, so it is a `New` parameter, not
  a fourth Host field: Host carries behaviors, not immutable data. 6th member of
  `uiPresentationOwners` — 140 `ui.*` sites including `ui.Region`/`ui.SetLiveSink`; the loop's output
  IS a twelve-hour incremental render, which is the one shape "return data and let the caller print
  it" cannot express. Three things worth reusing: (1) a smaller slice was measured and REJECTED — the
  engine and its organs are welded by 103 unexported identifiers at 179 sites, so any sub-slice would
  export them and un-export them again next commit; "smallest provable" was the whole engine here;
  (2) `commands.go` was a 2,504-line hand split across two disjoint spans with `cmdPrompt`/`promptLine`
  marooned between them, so every intra-cli split landed GREEN as pure reorganization BEFORE any
  `git mv` (the forkctl discipline, now proven twice); (3) five tag-gated scripted-e2e files reach
  into moving symbols but CANNOT move — they are welded to the shared process harness that also
  serves the fork/consult/delegate/preset families — so the package exports exactly six extra names
  for them (`StageRecord`, `PeerRecord`, `ReadPeerRecords`, `IterationCommand`, `LoopWorkPrompt`,
  `LoopInterruptedExitCode`) and nothing more. `internal/cli/fork_loop.go` is retired: its loop locks
  went to `internal/loop`, `lockSessionProducer` beside its wrapper in `commands.go`, `runForkLoop`
  into `fork_cmd.go`.
- 2026-08-10 — **+1 package, +1 edge, +1 `uiPresentationOwners` grant: `internal/forkctl`**
  (`{"agent", "box", "config", "forkspace", "project", "runtime", "sessionsvc", "tasks", "ui"}`);
  `cli` GAINED `forkctl` and **dropped nothing** — every package on cli's line survives in files that
  stayed. The fork/fleet CONTROL PLANE (~8.7k lines with tests): supervision (claim/stop/reap/detach/
  logs), listing + status, the review dossier and gate, the ff-only land, the declarative fleet, and
  the live board. This seam pointed the OTHER WAY from the previous four extractions — 66 outbound
  identifiers against 33 inbound (19:1) — so the naive whole-family move would have needed a ~20-field
  `Host`. The fix was a cut line, not a veto: SIX launch-spine functions stay in `internal/cli`
  (`forkCreate`, `forkACP`, `forkLaunchCmd`, `parseForkCreate`, `runForkLoop`, `fleetUp`, plus the
  dispatchers `cmdFork`/`cmdFleet` beside them, as `fork_cmd.go`), which is what collapses the Host to
  **three** fields (`EnsureRuntime`, `RunWatchLoop`, `ForkCost`) — cli owns LAUNCH, forkctl owns
  LIFECYCLE. `forkctl` needed **zero new transitive dependencies**; the deliberate
  `forkctl → sessionsvc` edge (the review-identity pair) is acyclic and adds nothing cli didn't reach.
  It is the **5th member of `uiPresentationOwners`** — 135 `ui.*` call sites including a full
  alt-screen TUI (`coop fleet watch`), the same first-class-verb-family reasoning `tasks` used. Three
  files were HAND splits, not `git mv`s (`fork.go` 1,422 → 747 stay / 764 move across 47 interleaved
  declarations; `fork_loop.go` and `fork_fleet.go` likewise), so they landed GREEN as pure intra-cli
  reorganizations BEFORE any file moved — two verifiable states in one commit. Two risk shapes worth
  reusing: (1) inbound was NOT tag-invariant — `scripted_fork_process_e2e_test.go` and
  `scripted_detached_process_e2e_test.go` reach into mover symbols only under `providere2e`, so the
  census ran under all five tag sets and the gate adds `go vet -tags providere2e ./...`; (2) `gitOut`/
  `padRight`/`pathExists`-class helpers were local-redeclared (the `internal/tasks` precedent), which
  left `internal/cli` holding several now-dead copies — deleted in the same commit rather than left
  for staticcheck to find later.
- 2026-08-10 — **no graph change**, recorded because two package CONTRACTS widened under a frozen
  table. `forkspace` absorbed the signing-policy and driver-neutralizer helpers
  (`WantsSigning`/`TrustedSignArgs`/`DriverNeutralizer`) beside `GitHardening` — pure `os/exec` +
  `strings`, so the leaf is still `{"processidentity"}` — and `ui` absorbed the one shared
  destructive-confirmation gate (`DestroyGate`/`Confirm`), so `ui` now READS the terminal it
  detects, not just writes it. Neither added an edge: both are leaves and every consumer already
  held the edge. Consequence for the next extraction — a package that owns a destructive verb does
  NOT earn the `ui` grant for the gate alone; it injects the decision through `DestroyGate`'s `asks`
  callback ([[destructive-confirm-gate]]). Also corrected this card's stale invariant text, which
  still listed three `uiPresentationOwners` after `tasks` became the fourth below.
- 2026-08-10 — **+1 package, +1 edge, −1 edge, +1 `uiPresentationOwners` grant: `internal/tasks`**
  (`{"box", "config", "forkspace", "project", "taskstate", "ui"}`); `cli` GAINED `tasks` and
  **DROPPED `taskstate`**. The largest of the 2026-08 extractions (~20.5k lines): the folder-mode
  task/backlog queue (`taskcmd.go`, `tasks.go`, `taskwatch.go`, `backlog.go`, `taskdir.go`), the
  claim/lease/ref authority registry (`tasklease.go`), the completion-window journal
  (`completionwindow.go`), the trusted completion audit (`controller.go`, moved WHOLE — census found
  no internal loop-engine content in it at all, only audit/lease vocabulary), and the ref-authority
  window sliced out of `fork_loop.go` (`lockRefAuthority`/`enterRefAuthorityWindow`, NOT the whole
  file — `lockLoopCheckout`/`lockSessionProducer`/the fork-lifecycle bulk stay in `cli`). `cli`
  losing `taskstate` is the interesting half: `taskdir.go` was the only cli file importing it.
  `internal/tasks` needed **zero new transitive dependencies** (same shape acpctl found). It is the
  **4th member of `uiPresentationOwners`** (alongside `box`, `cli`, `scaffold`) — a deliberate,
  reviewed exception to the normally-3-member list: `taskcmd.go`/`tasks.go`/`taskwatch.go`/
  `backlog.go` are a complete first-class CLI verb family (a live TUI board included), not a handful
  of narration call sites the way sessionsvc/acpctl's injected `Warnf` sufficed for. Two genuinely
  new risk shapes this extraction had that sessionsvc/acpctl/ladder/forkspace didn't: (1) two
  `providere2e` tests (`scripted_process_e2e_test.go`, `scripted_loop_watchdog_process_e2e_test.go`)
  reached directly into unexported mover symbols to compute/assert a spawned binary's on-disk state
  — caught only by an all-tags-union census, not a default-tag scan, and fixed by exporting the
  handful of symbols they needed; a third file (`scripted_loop_review_process_e2e_test.go`) turned
  up the same way once `go test -tags providere2e -c` (not `go build`, which never compiles
  `_test.go` files regardless of tags) was run. (2) `internal/tasks` had no `TestMain` of its own —
  `internal/cli`'s `TestMain` gave every test in the merged package a hermetic
  `COOP_TEST_LEASE_AUTHORITY_ROOT`; without an equivalent in the new package's own test binary, its
  tests would have silently read/written the real `~/.local/state/coop/task-leases`. Identifier
  exports were MOSTLY mechanical caps (`TaskLease`, `TaskCounts`, `TaskAssignment`,
  `TaskLeaseOwner`) despite the Stage 1 ruling asking for de-stuttering — only `taskItem` → `Item`
  (the highest-traffic one) was actually de-stuttered; this inconsistency is flagged for the lead
  as a deviation, not silently resolved. Full report in the task's own `log.md`/`state.md`
  (`2026-08-09-extract-tasks-lease-and-completion-audit-into-a`).
- 2026-08-10 — **+1 package, +1 edge, −1 edge: `internal/acpctl`**
  (`{"acpproxy", "agent", "box", "config", "ladder", "liveprocess", "preset", "processidentity"}`);
  `cli` GAINED `acpctl` and **DROPPED `processidentity`**. The ACP CONTROL PLANE — editor selector
  injection, provider/account/preset live switching, box respawn with context carry, limit-wait
  policy — came out of `internal/cli/acpcontrol.go` + its `acp_process_*.go`/`acp_reload_*.go`
  siblings (~6,830 lines with tests). `cli` losing `processidentity` is the interesting half: only
  the moving `acp_process_live.go` touched it (the live-tag process-identity gate for the ACP
  supervisor's isolated binary), so nothing cli-resident needed the edge once that file moved.
  `acpctl` needed **zero new transitive dependencies** — the first extraction where this held: every
  package on its line was already imported by the moving files today (`ladder`/`forkspace` from the
  two immediately-preceding extractions had already paid for what would otherwise have been
  injected). What DID need injecting (rotation.go's `accountsFor`/`expandLadder`,
  fusion_council.go's `resolveACPFusionCouncil`, modelscache.go's `writeModelsCache`, and — until
  2026-08-10 — ratelimit.go's `waitUntilWall`) is POLICY that stays with cli, the same shape as the
  sessions extraction's three `Host` functions — a `Host` struct of 5 function values `acpctl.New`
  takes (FOUR since `waitUntilWall`/`sleepOrWake`/the tick cap were promoted into `internal/ladder`,
  where the loop and the ACP control now both call them directly), cli's `acpHost()` the one real
  implementation. `acpctl` refuses the `ui` edge (zero `ui.` references in
  any mover file — this control narrates nothing on its own); `uiPresentationOwners` is unchanged.
- 2026-08-09 — **+2 packages, +1 edge, −1 edge: `internal/sessionsvc`**
  (`{"agent", "box", "config", "forkspace", "ladder", "runtime", "session"}`) and
  `internal/testutil/gitrepo` (a leaf); `cli` GAINED `sessionsvc` and **DROPPED `session`**. The
  whole remote-sessions service — ~7.5k lines of `internal/cli/session_*.go` — moved out; only the
  `coop sessions` command wiring stayed. cli losing the store edge is the interesting half and was
  deliberate: `internal/session` was imported by the moving files and nothing else, so the remainder
  reaches the store's constants through `sessionsvc` (`MaxReviewErrorBytes`, `BoundedDetail`) rather
  than around it. `sessionsvc` refuses the `ui` edge — its four `ui.Warn` calls became an injected
  `Host.Warnf` that cli wires to `ui.Warn`, and the doctor command, the only thing that PRINTS a
  verdict, stayed in cli with `ui.Error`/`ui.OK`. `uiPresentationOwners` is unchanged. The two
  prerequisite leaves below are what kept the injected surface at three functions instead of the
  sixteen-method interface the seam map first priced: fork and rotation are IMPORTS now.
- 2026-08-09 — **+1 package, +2 edges: `internal/ladder`** (`{"agent"}`) and `cli → ladder`. The
  ROTATION LADDER's pure mechanics — the cursor over a run's rungs (live/cooling/advance/park on the
  soonest reset) and the limit CLASSIFICATION that decides whether provider output was a rate limit
  at all, prose regexes and ACP JSON-RPC signals alike — came out of `internal/cli/rotation.go`,
  `ratelimit.go`, and `acpcontrol.go`, so the sessions service can drive a rotation by IMPORT rather
  than by injected function (the second prerequisite of the sessions extraction, after `forkspace`).
  App-bound policy stayed in cli and is why the leaf takes no `config`, `box`, or `ui` edge:
  expanding a preset ladder against signed-in accounts, applying a rung to the config, the loop's
  caps and narrated sleeps, and the ACP protocol handling all print or read state. Its one dependency
  is `agent`, for `Target` (the rung) and the adapter-owned rate-limit signals.
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
