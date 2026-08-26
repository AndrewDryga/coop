# Changelog

## Unreleased

<!-- Add entries here as you ship; this heading is renamed to the version on the next release. -->

- **`coop init` targets the current scaffold only.** Re-init still fills missing current files and
  preserves every existing hook, symlink, and custom hooks path, but it no longer recognizes and
  rewrites pre-v8 hook bytes, root-anchored `.agent` ignore rules, or the retired `.agent/rules/`
  un-ignore. `MIGRATING.md` gives the one-time manual replacements for a direct pre-v8-to-v9 jump.

- **Fork session re-entry uses one exact hint.** Coop now reads only the current
  `.coop/session.<provider>.<account>` record. It no longer adopts provider-only fork records or
  guesses the latest Codex conversation by cwd; a missing or invalid hint starts fresh, and Codex
  records the uniquely new native ID after the run. Exact per-account resume and `--new` remain.

- **Task leases now have one host-only authority.** Coop no longer mirrors its flock and heartbeat
  into provider-visible `tmp/lease.lock` and `tmp/lease.json` files. Acquisition, contention,
  `busy`/`stalled` observation, crash recovery, completion receipts, and audit reopen all use the
  inode-verified registry under `~/.local/state/coop/task-leases/`; task `tmp/` is scratch only.

- **Fleet is removed in favor of direct detached fork loops.** Start each worker with `coop fork
  <name> <target|preset> --loop -d --tasks <path>`; use `coop tasks split <n>` for mechanical queue
  slices, `coop tasks watch` for merged progress, `coop fork ls`/`logs -f` for fork state and output,
  `coop fork stop <name>` to stop one, `coop fork rm <name>` to remove one, and `coop fork merge
  --all` to land all forks. Coop no longer reads `.agent/fleet.yaml`. Removing its manifest parser,
  batch lifecycle, live board, adapter badges, duplicated credential/target resolution, and cast
  leaves fork/session lifecycle ownership and the useful direct primitives unchanged.

- **Fusion mode is removed; its useful pieces are the normal composition model.** Run a reusable
  orchestration recipe directly with `coop <preset>` or `coop acp <preset>`. For an ad-hoc second
  opinion, add repeatable `--peer <target>` flags to an agent, loop, fork, or ACP run. Preset
  native/consult/delegate roles, role personas and ladders, read-only `coop-consult` continuity,
  explicit peer credential scoping, and ACP preset/provider/account switching remain. Removing the
  separate `fusion` grammar, mandatory governor prompt, council state, and duplicate resolution
  paths leaves one lead-and-roles implementation.

- **The security baseline is enforced locally and in CI.** Coop now builds with Go 1.26.6, runs a
  pinned `govulncheck` as part of the one `make check` recipe, and asks Dependabot to watch Go
  modules weekly. MCP configuration also rejects duplicate case-insensitive header names and
  competing inline/environment Authorization sources instead of producing provider-dependent auth.

- **Remote ACP uses the editor's existing SSH transport instead of a Coop TCP protocol.** Zed
  Remote Development can run `coop acp <target>` beside the remote repository, while a custom
  agent may invoke that same stdio command through `ssh -T`. Coop therefore adds no
  unauthenticated listener, socket bridge, client lifecycle, or second network trust boundary.
  The initial ACP target or preset is now required instead of guessing the first signed-in
  provider; the live Provider and Account selectors remain available after connection.

- **An ACP session now starts when the editor spells the repo path differently than git does.** Coop
  mounts the repo at `git rev-parse --show-toplevel`'s spelling, and inside the box — Linux, and
  case-sensitive — that is the only path there is. A macOS editor or shell holding
  `/projects/andrewdryga` for a repo git calls `/projects/AndrewDryga` therefore got
  `cwd does not exist on the machine running the agent` and no session at all. Coop now forwards its
  own spelling when the editor's `cwd` names the same directory. The match is by file identity, so a
  symlinked worktree path works too and two genuinely distinct directories on a case-sensitive host
  are never confused for each other.

- **A container runtime that stopped is now reported as a stopped runtime, not a missing image.**
  `image inspect` fails identically whether the image is absent or the daemon is gone, so a runtime
  that died mid-session told every box command to run `coop build` — a rebuild that could not have
  helped, while the image was present the whole time. Box commands, `coop loop`, and the fork merge
  gate now probe the daemon on that branch and surface the actionable "daemon isn't responding"
  message instead. The probe runs only when the image already looks absent, so a normal launch pays
  nothing for it.

## 8.1.0

- **The base box now ships both `jq` and the `jv` JSON Schema validator.** `jq` remains available
  from Debian, while the independently versioned `jv` CLI is pinned and built as a static Go tool
  for the box architecture. Run `coop build` or `coop update` to add it to an existing base image.

- **Remote-session read-only authority is now enforced by the repository mount.** A policy may set
  `repository_read_only: true`; Coop binds the value into the immutable policy digest, persists it
  across daemon restarts, and mounts the session's primary fork read-only for every ACP provider.
  Prompt instructions are no longer the only barrier between a conversation or investigation and
  an unconfirmed repository edit. Existing policies stay writable until they opt in.

- **A provider-limited escalated session can explicitly fail back to its first healthy rung.**
  `POST /v1/sessions/{session_id}/turns` accepts `rewind_target: true` for one turn, durably moves
  the session to rung zero before delivery, and clears a foreign native transcript on a
  cross-provider move. Ordinary turns still inherit the current target and `min_target_index`
  remains an upward-only floor, so the failback is explicit and cannot silently weaken an
  escalated correction.

- **Account failover no longer tries a credential Coop already knows cannot authenticate.** Rotation
  now excludes native login files that their adapter classifies as requiring another login, while
  retaining env-backed and opaque credentials whose validity Coop cannot inspect. Real ACP failures
  such as `authentication_failed` and an expired OAuth session now advance an automatic account
  ladder instead of leaking the provider's dead-end error, and known-dead accounts are omitted from
  the editor selector. A pinned or exhausted ladder tells the user exactly which shell-safe
  `coop login provider@account` command repairs it.

- **A remote-session turn crawling inside a provider's own retry loop is now audible.** Coop emits
  `provider.alive` — cumulative `frames` and `bytes` counters — at most once a minute while ACP
  frames keep arriving and nothing higher-level has been narrated in that window. `provider.backoff`
  only covers limits Coop's own target ladder acts on; a provider CLI retrying 429s internally never
  reaches the ladder, so that turn streamed and produced no events whatsoever, which is exactly what
  a dead turn looks like. An ordinary turn narrates none, a long tool call is not described twice,
  and a turn producing no frames at all still produces nothing — leaving a client's silent-turn
  deadline to mean what it says.

- **A remote-session turn can now name the ladder rung it is delivered on.** `POST
  /v1/sessions/{session_id}/turns` accepts an optional zero-based `min_target_index`, and that turn
  is delivered no lower than that rung of the policy's target ladder. It is the escalation a client
  needs to re-deliver a corrected turn, because the rung that produced the answer being corrected is
  not the rung to correct it on. A session already at or above the rung does not move; rotation
  continues upward from it as usual; and when every rung at or above it is cooling the turn fails
  with `rate_limited` naming the rung rather than falling back below it, which would reach the client
  looking exactly like an honored escalation. An index that names no rung of the session's ladder is
  refused at admission, naming how many rungs there are. Turns submitted without the field behave
  exactly as before, down to the idempotency-key hash an in-flight retry replays against.

- **Remote-session turns now retain provider-reported cost.** Coop captures ACP cumulative USD
  cost updates, converts them into durable per-turn deltas, and exposes the amount beside token
  usage so API clients do not need a separate pricing table to account for reported spend.

- **Remote-session policies can now withhold shared operational capabilities.** Policies may set
  `project_env: false` and `project_mcp: false` to keep the daemon's shared environment and MCP
  configuration out of a session while still projecting the selected model credential and trusted
  instructions. The choices are policy-digested, persisted with the session, and enforced on every
  cold or warm ACP turn; existing policies retain their current projection defaults.

## 8.0.0

- **The loop can no longer adopt a task a human is actively claiming.** `coop tasks claim` is a pure
  folder move that exits immediately — it held no lease and left no durable record that anyone owned
  the task. `assignLoopTaskOnly` prefers an already-in-progress candidate, found the task's lease
  flock free (nobody was holding it — a human's claim holds nothing), and adopted it: a 2026-07-25
  dogfood run did exactly this to a founder's claimed task, duplicating the work underneath them. The
  lease system itself was never the problem — the kernel flock is the right authority for a live
  PROCESS, and a human's claim has no process left to hold one once `claim` exits. The fix adds the
  missing primitive: a durable ownership record, `<key>.owner.json`, in the same host-only registry
  the lease authority's own siblings (`<key>.json` metadata, `<key>.reopen.json` audit authority)
  already live in, with the same strict validation and atomic writes. `coop tasks claim` writes it
  (source, user, host, claimed-at) BEFORE it moves the folder, so a claim is protected from the
  instant it starts rather than from whenever its move happens to land; a claim that then loses the
  move race rolls the record back instead of leaving a phantom owner behind. `assignLoopTaskOnly` now
  checks every candidate — in-progress or todo — for this record before ever taking its lease, and
  skips one that carries it exactly like a busy lease, naming the owner and the release command; the
  loop's own resumed work is unaffected, because its adoption path never writes the record in the
  first place. The record never auto-expires: no mtime, no PID liveness, no heartbeat age ever clears
  it, because none of those can tell "gone quiet for a good reason" from "abandoned" — guessing wrong
  between those two is the exact incident this fixes, and long, quiet, legitimate work must survive.
  Only an explicit lifecycle act clears it: `done`, `block`, `unblock`, and a new explicit
  `coop tasks release <id>` — the hand-back that clears the claim but leaves the task in
  `10_in_progress/` for the loop to pick up next. `coop tasks ls` now tags a claimed in-progress row
  "claimed by `<user>`" in place of its usual lease label, which would otherwise read the actively
  misleading "unleased." `.agent/kb/task-authority-model.md` now maps all four authorities over a
  task and its checkout — claim, lease, checkout, and ref — so nobody has to re-derive this shape.

- **A stray commit landing mid-iteration no longer destroys a finished task's completion.** `coop
  loop` validates a completion over its iteration's commit range, which is a time window on one
  branch — anything committed to the same checkout while a box is running joins that range,
  including unrelated work a human commits on the host. `unbindableTasks` rejected the WHOLE
  completion whenever any in-range commit carried a `Coop-Task` trailer for a task outside the
  finished set, so a human's own commit for their own, untouched task landing in that window could
  cost a finished, gated, 32-minute task (recorded in
  `.agent/kb/loop-range-rejects-outside-commits.md`). The guard is load-bearing — it stops a boxed
  agent from smuggling in a binding for a task it doesn't own — but was broader than its purpose. It
  now rejects only when a foreign binding names a task this iteration's authority consumption could
  actually touch: the finished set, the leased task id, the audit-reopen record's task, any task whose
  queue state this completion window observed change, or any task already archived before the box
  ever ran (the completion window's baseline snapshot, captured before launch) — a comparison set
  built entirely from host-side knowledge the box cannot influence. That last member matters even when
  the archived task's folder never moves: an already-closed task's history is meant to stay closed, so
  a forged extra commit for it is still rejected — a first cut of this fix missed that surface and
  wrongly tolerated it, caught by the existing `TestProviderScriptedLoopReviewProcess` e2e suite.
  Independent of the whole set, a foreign binding that REWRITES a task's already-landed commit (rather
  than adding a new one alongside it) still always rejects too, since that can only happen via a
  rebase or amend that reparents another task's history — silently altering its committed content
  without ever moving its queue folder. Anything else is tolerated: reported live (`ui.Warn`) and
  journaled to the named task's own `log.md` instead of destroying the completion, and that task's own
  reachable-binding count already refuses to let its real next completion silently ride on a binding
  it did not itself just create. The adversarial matrix — a forged trailer for the box's own task, for
  a second task it also completed or reopened this iteration, for an already-archived task it never
  touches, one that rewrites another task's binding, a multi-value commit, and an invalid binding — is
  still refused exactly as before.

- **A completion can no longer be trusted against history nobody validated.** The work loop reads
  `headAfter` once an iteration's box exits, validates the completion over `iterHead..headAfter`,
  then — many filesystem operations later — consumes task authority: finalizes the completion, writes
  its receipt, and (for host-authorized rework) removes the single-use audit-reopen record. Nothing
  serialized the repository ref across that gap: an interactive `coop run`, a host-side signing
  rewrite, `coop fork merge` landing onto the same checkout, or a human commit could all move `HEAD`
  in between, so a reviewed generation could be consumed while unvalidated history was already
  current. `lockLoopCheckout` closed the loop-vs-loop case but excludes everything else. A new
  per-worktree `lockRefAuthority` — keyed and located exactly like `lockLoopCheckout` (the resolved
  worktree path, under `$ConfigDir/.locks`, never the repo name, so a fork fleet stays fully parallel)
  — now covers the short validate→finalize→consume window only, never the box run itself: the first
  action inside it re-reads `HEAD` and fails closed — no receipt written, no audit-reopen generation
  consumed, the task left actionable in `10_in_progress/` with its commit intact — the instant it no
  longer matches the value that was validated. Every one of coop's own host-side ref-mutating paths
  now takes the same lock, so coop can never trip its own refusal: the loop's own signing sweep
  (`signUnpushed`), `coop fork merge`'s fast-forward onto the parent, and the host-authorized
  completion of audit-reopened work (`coop tasks done`, and fork-merge's queue reconciliation) — which
  shares the loop's exact validate-then-consume shape, just reached by a human instead of a box.
  Unlike `lockLoopCheckout`'s bare flock, the new lock is proved after the fact the way task-lease
  authority already is (`lockLeaseAuthorityWith`, `tasklease.go`): open, flock, then `fstat` the held
  descriptor against a fresh `lstat` of the name, so a lock file removed and recreated underfoot — a
  purge, a careless `rm -rf .locks` — can never leave two controllers each holding an "exclusive" lock
  on a different inode.

- _Internal restructuring, no user-visible change._ The `transport-bounds-do-not-abort-valid-work`
  rule (`.agent/kb/rules/`) is now gated instead of riding on review alone. It was created
  2026-08-02 after two same-day violations that killed working consults — a reintroduced timing
  contract, and a 1 MiB reply cap that discarded a consult which had already run to completion —
  but carried `check: none`, so nothing caught a third. `TestConsultWrapperBoundsDefaultToUnlimited`
  (`internal/fusion`) now asserts `COOP_CONSULT_STREAM_LIMIT` and `COOP_CONSULT_TIMEOUT` both
  default to unlimited, checked against the rendered consult wrapper's own fallback value and
  against its actual behavior with no override set; `make rules-check` runs it as part of the gate.

- **A Claude session credential that expires no longer takes the whole deployment down with it.**
  Every turn on a `coop sessions serve` target running Claude failed permanently once the host's
  ~8h Claude OAuth access token expired — two production deployments went dark this way on
  2026-08-08, each still holding a perfectly good refresh token nothing ever used. Codex's
  `LiveCredentialSpec` already renewed an expiring access token in trusted host storage before
  projecting the access-only copy a session box receives; Claude's declared `Portability` and no
  `Prepare`, so nothing renewed it — the in-box CLI has no refresh token by design, and the host
  never ran the refresh either, so `credential is not portable through the turn deadline` repeated
  on every turn until a human re-logged in. Claude now renews the same way: a host-side `Prepare`,
  serialized on the profile's own refresh flock, atomically persists the rotated token before
  projection. It talks to Claude Code's own OAuth endpoint directly rather than shelling out to the
  CLI to do the refresh — on macOS that CLI migrates a renewed credential out of
  `.credentials.json` into the login Keychain and deletes the file, confirmed by hitting exactly
  that while triaging this outage and recovering the credential from the Keychain by hand. Endpoint
  and client id are carried as verified constants with env overrides, the same shape Codex's
  refresh already uses; the request asks for exactly the scopes the stored login already holds. A
  credential whose refresh token is itself dead still fails with a sign-in diagnostic, not the
  portability one, and the refresh token itself still never crosses into the box.

- **Ctrl-C during box setup now aborts promptly, instead of waiting out whatever host step it
  landed on.** The loop's second Ctrl-C cancels the run's context, but `box.Run`'s host-side setup
  — projecting the repo and every generated mount, bringing sibling services up, inspecting the
  services network, assembling the container's arguments — never checked it: a wedged `compose up`
  or `network inspect` (both plain, uncancelable subprocess calls) held the whole run hostage until
  the syscall itself returned, however long that took. A cancellation is now checked at a boundary
  between each of those phases — including before the first one, so a Ctx already canceled when
  `box.Run` is entered aborts immediately — and the error names the step it aborted before. It
  still can't reach inside a call already in flight (there is no lever to pull mid-syscall without
  heroics this fix deliberately avoids), so the wait is now bounded by the SLOWEST single host step
  instead of the whole setup — and an aborted setup releases exactly what a normal completion would
  have: temp files and mounts through the same deferred cleanup, and a review run's disposable
  Compose project through the same teardown its success path already runs. It also never signals
  the runtime-launch boundary, so the provider watchdog armed there never arms for a launch that
  didn't happen. Every other caller leaves `Ctx` nil, so nothing changes for them.

- **A turn's token usage now actually reaches the session API.** Coop has recorded what every turn
  cost since per-turn usage shipped — four columns on `turns`, filled from the ACP prompt result —
  but the public `TurnDTO` had no field for them and `publicTurn` copied none, so
  `GET /v1/sessions/{id}/turns/{turn_id}` answered with no `usage` object at all while the database
  held real numbers, and the caller that asked what a turn spent got a blank where the spend belongs.
  The durable record and the public wire type are two hand-maintained projections with nothing tying
  them together, which is how a field can be correct in the store, the schema, the scanner and the
  ingest path and still be invisible to every client. `usage` — `input_tokens`,
  `cached_input_tokens`, `output_tokens`, `reasoning_tokens` — is now published on the single turn,
  the turn list, and both turn mutation responses. It stays `omitzero`, exactly as the record carries
  it, so a turn no provider measured publishes no `usage` object at all rather than four zeros a
  caller would price as a free turn.

- _Internal restructuring, no user-visible change._ The **loop engine** — the unattended work engine
  behind `coop loop` and every fork worker: the drain, the work/between/signoff stages, the review
  pipeline and its host-owned verdicts, the provider watchdog, the stream-JSON decoders, the
  rate-limit caps and rotation, the live bar, and the run telemetry — moved out of `internal/cli`
  into a new `internal/loop` (~7.1k non-test lines, 47% of what `internal/cli` was carrying; ~13k
  with tests). Last and largest of the 2026-08 extractions, and the one the whole sequence was
  ordered around. Measured before it was moved, the loop turned out to be a **sink**: only seven
  production call sites in `internal/cli` reach into it, so nothing else is built on top of it —
  which made the whole question what it needs, not who needs it. It needs three functions
  (`SweepOrphanBoxes`, `SignUnpushed`, `BuildRotation`) plus the version string `-ldflags` pins to
  `internal/cli`. The seam is the same cut line the fork extraction drew, one level up: **cli owns
  the COMMAND** (argv to preset, target, peers, queues, rotation, image — now
  `internal/cli/loop_cmd.go`) and **loop owns the RUN**, which is what keeps the injected surface at
  three functions instead of eleven. `coop loop`'s eleven-parameter internal call became a `RunSpec`
  struct, and with it three fields on the CLI's shared `app` struct turned out to be loop-only and
  left with the engine — a fourth, the fork's container owner, stopped being a field the fork
  launcher set and restored with a `defer` and became a value it passes in. Every banner, prompt,
  warning, receipt, telemetry row, exit code, and live-bar frame is byte-identical (verified by
  extracting every string literal in both packages before and after: the only differences are the
  private git/format helpers the new package redeclares locally, exactly as `internal/tasks`,
  `internal/sessionsvc` and `internal/forkctl` already do).

- _Internal restructuring, no user-visible change._ The **fork/fleet control plane** — supervision
  (claim, stop, reap, detach, logs), `fork ls`/`status`, the review dossier and its isolated gate, the
  fast-forward-only land, the declarative fleet, and the live `coop fleet watch` board — moved out of
  `internal/cli` into a new `internal/forkctl` (~8.7k lines with tests). Unlike the four extractions
  before it, this family is not an engine sitting under the CLI; it sits at the same altitude, so the
  seam is a cut line rather than a lift: **cli keeps LAUNCH** (opening or resuming a fork — preset,
  one-off target, image, peers, `box.Run` — plus the `coop fork`/`coop fleet` dispatchers, now
  `internal/cli/fork_cmd.go`) and **forkctl owns LIFECYCLE** (everything a fork needs once it
  exists). That line is what keeps the injected surface at three functions instead of twenty. The
  fork's on-disk contract stays one level below in `internal/forkspace`, unchanged. Every command,
  every message, and every exit code is byte-identical, the fleet board included.

- _Internal restructuring, no user-visible change._ Two pieces of shared plumbing that had already
  drifted into duplicates moved to their one right home, ahead of the fork/fleet extraction that
  would have minted a third copy of each. The **git signing policy and driver neutralizer** —
  `WantsSigning` (do you sign commits?), `TrustedSignArgs` (the vetted `-c` flags that re-enable
  signing after the hardening turns it off), and `DriverNeutralizer` (blanks every filter/merge/diff
  driver an agent planted in a repo's local config, before a rebase checks the tree out) — moved from
  `internal/cli` into `internal/forkspace`, beside the `GitHardening` list they are the documented
  other half of. The leaf is still a leaf: they need only `os/exec` and `strings`. The
  **destructive-confirmation gate** — the one prompt every unrecoverable delete routes through — had
  TWO identical copies (`internal/cli` and `internal/tasks`) and is now one, `ui.DestroyGate`, in the
  package that owns the terminal: a confirmation is the one thing a command cannot return as data for
  its caller to print, because the answer has to come back from the same tty. Same prompt text, same
  default (No), same refusal when there is no terminal to ask; both helpers' tests moved with them.

- _Internal restructuring, no user-visible change._ The **task-authority engine** — the folder-mode
  task/backlog queue (`taskcmd.go`, `tasks.go`, `taskwatch.go`, `backlog.go`, `taskdir.go`), the
  claim/lease/ref authority registry (`tasklease.go`), the completion-window journal
  (`completionwindow.go`), and the trusted completion audit (`controller.go`, moved WHOLE — it turned
  out to hold no loop-engine content at all, only audit/lease vocabulary) — moved out of
  `internal/cli` into a new package, `internal/tasks` (~20.5k lines with tests, the largest of the
  2026-08 extractions). The ref-authority window (`lockRefAuthority`/`enterRefAuthorityWindow`) moved
  with it, sliced out of `fork_loop.go`, because a mover file (`tasks.go`'s `done` verb) calls it
  directly; `lockLoopCheckout`, `lockSessionProducer`, and the rest of the fork-lifecycle machinery
  stayed. The CLI kept the loop engine's own box-spawn/provider-rotation orchestration and every
  command's thin dispatch. `internal/tasks` is the fourth (and a deliberately reviewed exception to
  the normally three-member) `uiPresentationOwners` grant — the task/backlog verb family is a complete
  CLI surface, live board included, not a handful of narration call sites. The on-disk registry
  formats (lease/claim/audit-reopen/departure records, the completion-window journal) and every
  `coop tasks`/`coop backlog` verb's output are byte-identical. `.agent/kb/task-authority-model.md` is
  now also `internal/tasks`'s package doc, and gained the lock-ordering invariant it implied but never
  stated: ref authority is acquired before lease authority, never the reverse.

- _Internal restructuring, no user-visible change._ The **ACP control plane** — editor selector
  injection, provider/account/preset live switching, box respawn with context carry, and the
  limit-wait policy — moved out of `internal/cli` into a new package, `internal/acpctl`
  (`acpcontrol.go` → `control.go`, `acpwarm.go` → `warm.go`, the process/reload build-tag pairs, and
  their tests — ~6,830 lines). The CLI kept `coop acp`'s command wiring: argument/preset/governor
  parsing, the supervisor's signal handling and image build, and the one spawn path (`spawnBox`) that
  execs every inner box. What the control genuinely cannot own for itself — rotation's account/ladder
  expansion, fusion council resolution, the opportunistic models-cache write, and the suspend-safe
  rate-limit wait — is injected as five functions on a `Host`, the same shape the sessions extraction
  used, needing zero new transitive dependencies: every package `internal/acpctl` imports was already
  imported by the moving files today. Every wire behavior is unchanged, and the tests moved with the
  code.

- _Internal restructuring, no user-visible change._ The **remote-sessions service** — policy
  authority, the HTTP v1 API and its socket, one-turn ACP execution, workspaces, review, companions,
  sources, outputs, and artifacts — moved out of `internal/cli` into its own package,
  `internal/sessionsvc`. The CLI kept the `coop sessions` command wiring: flags, path defaults, the
  serve loop that owns signal handling, and `sessions doctor`, which is the only part that prints.
  What the library genuinely cannot own for itself is injected as three functions on a `Host` — the
  merge-policy scan, the review gate built from this repo's merge image, and a warning line on the
  terminal — because the fork on-disk contract and the rotation ladder had already become leaf
  packages (`internal/forkspace`, `internal/ladder`) it can simply import. Every behavior, wire
  format, and bound is unchanged, and the tests moved with the code.

- _Internal restructuring, no user-visible change._ The **rotation ladder's mechanics** — the cursor
  over a run's rungs (which is live, which is cooling, when to advance, and parking on the soonest
  reset when every one is limited) and the **limit classification** that decides whether provider
  output was a rate limit at all (the prose regexes, the reset-time parser, the backoff bounds, and
  the ACP JSON-RPC signal matching) — moved out of `internal/cli` into a new leaf package,
  `internal/ladder`. Building a ladder from a preset against the signed-in accounts, applying a rung
  to the config, the loop's caps and narrated sleeps, and the ACP protocol handling all stayed in the
  CLI, which is why the leaf imports only `internal/agent`. Every regex, bound, and decision is
  byte-identical, and their tests moved with them.

- _Internal restructuring, no user-visible change._ The fork **on-disk contract** — where a fork's
  workspace lives, what a fork may be named, the `<repo>-forks/.coop` state directory, the lifecycle
  flock, the pidfile wire format and its identity doctrine, and clone/destroy/pin — moved out of
  `internal/cli` into a new leaf package, `internal/forkspace`. Worker supervision (signalling,
  killing, reaping, detach orchestration, container teardown) stayed in the CLI, which is why the
  leaf prints nothing and imports only `internal/processidentity`. The pidfile format is
  byte-identical.

- **The internal dependency graph is frozen in the gate, so an import can no longer become
  architecture by accident.** The graph was already a clean DAG — nothing importing the CLI,
  presentation at the edges — and nothing enforced it, which is how `internal/agent` had picked up a
  `ui` dependency for one badge color and `internal/scaffold` an entire `box` dependency for one
  path. `internal/importdag_test.go` now freezes the exact edge set (20 packages, 40 edges) as a
  table you can read as the architecture diagram, and diffs the tree against it BOTH ways: an
  unplanned edge fails, and so does an edge the table still expects but the code has dropped, so the
  table can't rot into a description of a graph that used to exist. Two invariants sit above the
  table and hold however it's edited — nothing imports `internal/cli`, and `internal/ui` is imported
  only by the granted presentation owners (cli, box, scaffold) — so the fix for a violation is never
  "add a row". Every failure names the offending edge and both files a deliberate change has to
  touch: the table and the rule card (`.agent/kb/rules/internal-import-dag.md`) that explains why.
  Stdlib only (`go/parser`), build-tag and GOOS agnostic, and it runs in the normal `go test ./...`
  the gate already does.

- **A box whose coop was killed is reaped by the next run in that checkout, instead of burning
  tokens until somebody notices it.** `--rm` never fires when the host coop is SIGKILLed (or the
  machine reboots mid-run), and nothing watches a box from the outside, so an orphan kept its
  provider session alive with no host-side observer at all — and the six orphan fixes before this
  one each patched a single pull point instead of making an orphan *identifiable*. Every box now
  carries `coop.host=v1:<workspace>:<pid>:<start-token>`: the identity of the host process that
  would have removed it. Loop start, `coop fork <name>`, `coop fleet up`, and `coop build`'s recycle
  read that label for the repo they run in and remove exactly the boxes whose recorded supervisor is
  provably dead — the kernel says that pid is gone, or it now belongs to a different process. It is
  the same evidence `coop fork stop` already demands before it signals anything, and nothing else
  counts: a live supervisor, an identity coop cannot verify, and another checkout's box are all left
  alone, and no box is ever judged by its age, image, or name. A box started by an older coop carries
  no such label, so coop cannot tell whose it is — `coop doctor` REPORTS those (with the ids and the
  label evidence behind every finding) and the sweep never touches them.

- **A coop killed mid-start or mid-land no longer wedges its fork until a human runs `coop fork
  stop`.** Two crash windows left state only a person could clear. Starting a detached loop reserves
  the fork's pidfile with O_EXCL BEFORE the worker exists — that reservation is what stops two loops
  racing one worktree — but a coop killed in that gap (Ctrl-C during `coop fleet up`, a host crash)
  left it behind, and every later start of that fork refused with "still needs box cleanup" over a
  tombstone that owned nothing at all. Separately, `coop fork merge` rebases the fork inside its own
  clone and aborts a rebase that FAILS — not one interrupted by coop's own death — so a crash
  mid-land left `.git/rebase-merge/` in the fork, and every later merge died on the leftover state
  instead of recovering it.

  Both recover now, on exactly the evidence coop already demands before it will signal a worker: a
  recorded pid plus its kernel start token. A reservation carries the identity of the coop process
  that made it, so a later start can PROVE that owner is gone — the pid is dead, or now belongs to a
  different process — reclaim the fork, and say so. A merge that finds leftover rebase state runs
  `git rebase --abort` and continues, naming what it found and what it did. Nothing is inferred from
  file age or mtime: an owner that is alive, one whose identity can't be read, and — for a
  reservation — one that died in the instant AFTER forking its worker (which may still be looping
  unrecorded) all still refuse, naming the owner and `coop fork stop <name>`. An abort that fails
  carries git's own reason plus the commands to finish it by hand.

- **BEHAVIOR CHANGE: the provider-attempt watchdog is armed by default, so one wedged provider can
  no longer hold an unattended run forever.** Every deadline has shipped at 0 — disabled — since
  "stop killing models that are still working", and that correction was right: a clock cannot tell a
  LONG attempt from a WEDGED one, and coop had killed real work with one (a bounded consult died
  mid-security-review; a delegate was killed ten minutes in). What it left behind was the opposite
  hole. Every retry budget in the loop counts outcomes that already HAPPENED, so nothing at all
  bounded an attempt that never finished: a hung container start or a silently wedged provider CLI
  turned an overnight drain — or a detached fork nobody was watching — into an indefinite stall that
  only Ctrl-C or `coop fork stop` could end, with the loop slot, the task lease, and the credential
  held for as long as it lasted. A built-in loop/review/preflight attempt is supervised again: **10
  minutes** to its first model action, **30 minutes** between recognized model actions, **2 hours**
  on any one foreground tool, under a non-resettable **48-hour** attempt ceiling. There is no new
  knob and no off switch — the shorten-only internal override is the only escape hatch, by design.

  Those are SILENCE budgets, not service-level targets, and they only supervise now because the
  three fixes before them landed first: the start deadline is armed at the runtime-launch boundary
  (coop's own host setup is never charged to the provider), only semantically valid stream events
  re-arm anything (an empty envelope is not activity, per-attempt state is bounded, and the ceiling
  is reachable by no event at all), and a provider whose stream cannot report tool calls gets one
  conservative 2-hour post-progress budget instead of an idle deadline it could never suspend. An
  open foreground tool still suspends the idle clock, so the 40-minute `make check` coop promises to
  let finish still finishes. A fire kills that attempt and nothing else: any completion it wrote is
  restored, the task stays actionable, the rotation advances without cooling the abandoned rung, and
  three consecutive timeouts on one stage stop the run rather than looping. The warning now also
  names the deadline that fired and the silence it observed — "timed out (provider_idle_timeout)
  after no recognized provider activity for 30m1s (idle deadline 30m0s)" — so a deadline that is too
  tight for your repo is visible rather than inferred. The trade is deliberate and it is not free: a
  false kill costs one restarted task, while no kill at all costs the whole unattended run.

- **A provider whose stream cannot report tool calls is no longer supervised as if it could.** The
  attempt watchdog suspends its idle deadline while a foreground tool is open — that is how a
  40-minute `make check` survives a 30-minute silence budget — but it only works for a stream that
  says a tool started. Grok's does not. Probed against the installed CLI at v0.2.101, a run that
  shelled out emitted `thought`, `text`, and `end` and nothing else: no tool start, no tool end, no
  id to pair them with. A grok attempt sitting on a long gate was therefore indistinguishable from a
  grok attempt that had died, and the idle deadline would have killed the gate coop promises to let
  finish — while the docs claimed long tools survive. Adapters now **declare** what their stream
  proves about tools, and the watchdog selects policy from that declaration rather than inferring it
  from process names, CPU, or the bytes seen so far: claude, codex, and gemini pair every tool start
  with an end under a provider id and keep the shipped idle-plus-tool-cap supervision unchanged;
  grok declares none and gets one conservative post-progress deadline instead — 4× whatever idle
  budget is armed, so a 2-hour fallback wherever the 30-minute idle deadline applies — and no tool
  cap, since no event of its schema could ever arm one. Long enough that no honest gate reaches it,
  short enough that a silent attempt is still bounded, and derived from the idle budget rather than
  hardcoded, so the shorten-only override shortens it too and the non-resettable attempt ceiling
  stays the outermost bound. The
  watchdog also refuses tool events from a stream that declared none, because nothing may suspend a
  deadline whose resuming event does not exist; a stream nobody has probed reads as "no tool
  lifecycle" — the conservative side — and the registry's own test fails rather than let it ship
  undeclared. Every deadline was still disabled when this landed; it is the per-provider correctness
  they needed before they could be armed by default, which the entry above does.

- **The provider watchdog now treats the stream it supervises as what it is: untrusted input from
  the box.** Those bytes arrive on the stdout of the container that holds the credential and runs
  the agent, and any process in there that reaches that descriptor can write provider-shaped
  events — so "the adapter recognized this event type" was never the same thing as "the model did
  something". Three things changed. **Empty envelopes stopped counting:** an assistant turn with no
  content, a Codex item with no id, a Gemini delta with no text, a Grok event with no data — each
  parsed, each named a type coop knows, and each bought a free deadline reset. An event now has to
  carry the fields that make it mean something, and a tool only suspends the idle deadline when it
  names both the tool and the id its result will arrive under. **Per-attempt state stopped growing
  with invented ids:** open tools are capped at 64 and decoder labels at 256, with the overflow
  dropped rather than evicted — evicting the oldest open tool would re-anchor the absolute tool cap
  to a younger one, which is exactly the endless extension the cap exists to prevent. **And the
  honest part:** none of that makes a stream truthful, because a forged event that carries content
  is indistinguishable from a real one. So the watchdog now also runs one clock no event can touch
  — a non-resettable attempt ceiling, armed once at the runtime launch, 24× the longest deadline
  the operator armed, reported as `provider_attempt_timeout` and retried under the same
  three-in-a-row cap as the rest. It is derived rather than configured, so nothing can lengthen it —
  48 hours at the deadlines the entry above arms. Separately, the
  internal `COOP_PROVIDER_TIMEOUTS` override may now only SHORTEN supervision, never lengthen it
  past what coop ships, never set a sub-second deadline that kills healthy providers at launch —
  and it says on stderr exactly what it clamped and why, because a bound nobody chose and nobody
  can see is worse than none.

- **A slow box startup is no longer charged to the provider as silence.** The provider-attempt
  watchdog armed its start deadline before `box.Run` — but `box.Run` does substantial host work
  first: projecting the repo and every generated mount, bringing sibling services up, inspecting
  the services network, assembling the container's arguments. A host that took longer than the
  start deadline to get through that produced a `provider_start_timeout` against a box which had
  launched nothing. The loop then rotated a healthy target away, spent one of its three
  consecutive-timeout retries, and re-read the whole task on the next rung — all for coop's own
  slowness. The mislabel also could not act on itself: a fired deadline's only lever is cancelling
  the box context, and synchronous host setup never reaches the cancelable launch, so the timeout
  waited out the very work it had already blamed. The clock now starts at the runtime-launch
  boundary: `box.Run` signals its caller once, after all host setup and immediately before the
  container starts, and the watchdog arms there — exactly once per attempt, never before, and not
  at all for a run that fails before it launches anything (which now reports its real error).
  Silence is measured from the launch, and a deadline can only ever cancel a launch it can reach.
  All three deadlines still default to disabled, so nothing changes today; this is the correctness
  they need before they can be armed by default.

- **The registry that decides whether a task is really finished no longer lives in a cache
  directory.** Coop keeps host-global completion trust outside any repo: the lock files whose
  kernel flocks make one controller the single writer for a task, the completion receipts that
  prove a `99_done/` folder got there through host authority, the audit-reopen records that
  authorize reopening shipped work, and the completion-window journal that crash recovery replays.
  All of it sat under `~/Library/Caches/coop/task-leases` (`$XDG_CACHE_HOME` on Linux) — a
  directory the OS is entitled to delete: macOS purges `~/Library/Caches` under disk pressure and
  every "clean up my Mac" tool empties it on request. A purge **mid-run** unlinks a lock file whose
  descriptor a controller still holds, so the next controller recreates that name as a fresh inode
  and both hold an "exclusive" lock on a different one — the single-writer invariant dissolves with
  nothing printed. A purge **between runs** erases the receipts, degrading crash recovery to
  restore-and-redo and stranding audit-reopened tasks behind manual repair. The registry now lives
  at `~/.local/state/coop/task-leases/v1`, alongside the session store's state root, on both macOS
  and Linux. **The move happens once, automatically:** the first command run by the new binary
  takes a lock and adopts a populated cache registry into the new location — a whole-directory
  rename on the same volume, or a per-file fsync-then-rename copy across volumes — and the old
  cache path is gone afterwards. Nothing ever reads it again; there is no permanent fallback,
  because a fallback would keep the deletable path load-bearing forever. Receipts, reopen records,
  departure records and the window journal survive byte-identical (their filenames are hashes of
  repo plus task id, which the move does not touch). **One upgrade caveat:** a `coop` process that
  was already running when you upgraded holds its locks on *file descriptors*, not paths, so it
  keeps using the old inodes and is not mutually exclusive with a new-binary process for the rest
  of its life. Let in-flight runs finish before upgrading, or stop them — the window closes when
  the last pre-upgrade process exits. Separately, **every authority lock is now proved after it is
  taken**: Coop re-stats the descriptor against the name it locked and, if they no longer match,
  drops the lock and retakes it on the inode that currently answers the name (bounded retries, then
  a clear error). A lock file deleted underfoot is now detectable instead of silently non-exclusive.

- **One gate: `make check` and CI's check job are now the same recipe.** They were two step lists
  maintained by hand, and they had drifted in BOTH directions. Only CI ran the race detector,
  `go build ./...`, ShellCheck, and a version-pinned Staticcheck; only a laptop ran the cast
  hygiene scan, the rules-card validator, the maintenance-tool tests, and the tagged
  process-control race tests. So a data race or a build break in code no test imports could not
  fail locally, a malformed rules card could not fail CI, and main once rotted red with no local
  gate able to see it. CI's check job now installs the pinned tools and calls `make check`, and
  `make check` runs the union — a new check goes in the Makefile, never into the workflow.
  Missing tools no longer soft-skip themselves out of the gate: Staticcheck, ShellCheck, and
  python3 each fail with the one-line command that installs them, and a Staticcheck that isn't
  the pinned version fails the same way instead of quietly linting by different rules. That pin
  (`STATICCHECK_VERSION` in the Makefile) is the single place — CI reads the version from there
  rather than repeating it. **The box image now ships ShellCheck**, since the in-box gate runs the
  same recipe; run `coop build` once to pick it up. The two container-boundary jobs (the doctor
  runtime matrix and review-writes) stay separate CI jobs and `make check` stays
  runtime-independent — a comment on the target says so, instead of the old claim to be "what CI
  runs". Expect a slower local gate: the race suite is what a race detector that actually gates
  costs, and it runs last so cheaper failures still surface first.

- **A crash can no longer blank the file that says which account to use.** The default-credential
  pointers under `~/.config/coop` decide which subscription a run — including an overnight `coop
  loop` — signs in with, and writing one was best-effort in two ways. The lock around the
  read-modify-write was advisory in the loosest sense: when the lock file couldn't be opened or
  flocked, the write went ahead anyway, so two concurrent `coop credentials <agent> <credential>
  default` runs could silently drop one of the two edits. And the write renamed a temp file into
  place without ever fsyncing, so a power cut could leave the pointer file present but EMPTY — which
  reads back as "no default set", quietly moving the next run onto a different account and spending
  the loop's rotation budget on auth failures nobody ordered. Both are closed. An unobtainable lock
  now refuses the write and returns an error naming the lock file, instead of proceeding unlocked
  (flock on a local file effectively never fails on a healthy system, so nothing changes in normal
  use). The atomic write fsyncs the file before the rename and its directory after, so a crash
  leaves either the old pointer or the new one and never a blank — the same durability the
  refreshed Claude and Codex credentials written through that path now inherit. True caches keep
  the light path on purpose: losing the model catalog or the daily update check to a power cut
  costs one refetch, and each now says so where it's written.

- **A fork that lands but can't be reconciled now says so, instead of costing you the work twice.**
  `coop fork merge` moves every parent-queue task whose `Coop-Task` trailer just landed to done, so
  the parent loop doesn't redo it — but it read that trailer list through a helper that returns an
  empty string on ANY git failure. An unreadable range and a fork that landed no tasks were therefore
  the same answer: reconcile nothing, report a clean land. The landed tasks stayed in `00_todo/`, and
  the next loop iteration redid work already sitting in history. That read now fails loudly. The
  merge is never rolled back for it — the commits stuck, only the bookkeeping is missing — so the
  command prints its `landed <fork>` line, then an error naming the fork, the range, and the two
  commands that reconcile it by hand, and exits nonzero. A land whose reconciliation failed also
  keeps its fork workspace instead of deleting it: nothing gets destroyed right after an unexplained
  failure.

- **`coop loop` stops on an unreadable HEAD instead of mis-counting progress.** The loop treats a new
  commit between iterations as progress, and validates every task completion against a commit range —
  both off `git rev-parse HEAD`, whose failure read as an empty string. A broken repo looked exactly
  like an iteration that committed nothing, quietly spending the stall budget on iterations that
  could never bind a completion anyway. The loop now refuses to start when it can't read HEAD, and an
  iteration whose HEAD read fails stops the run with git's own message plus the recovery — in-progress
  work is resumed on the next run, nothing is lost. Display-only git reads are untouched: an empty
  answer is still fine when nothing depends on it.

- **`coop acp` builds the box image when it's missing, instead of dying.** A pruned `coop-box`
  took ACP down with no usable explanation: each of the four warm-target children failed with
  "image not built", the proxy burned its five-attempt rapid-fail cap in half a second, and the
  editor showed `agent exited 5 times within 2s; giving up` — the actionable line was buried in
  the editor's agent log. The supervisor now checks once, before any child spawns or the warm
  pool fans out, and builds if there's nothing there. A present image costs one existence check;
  this is a "can anything run" guard, not a freshness check. The build's output goes to **stderr
  only** — on this path stdout is the JSON-RPC wire and stdin carries the editor's requests, so
  the build must touch neither. Expect a slow first connect, narrated in the agent log; a build
  that fails returns one actionable error rather than a respawn loop.

- **BREAKING: the rules KB moved to `.agent/kb/rules/`.** Rules and the descriptive kb were
  siblings, so the same normative note lived at a different path in every repo — and a rule
  couldn't move between them without a rewrite. There is now ONE committed knowledge tree:
  descriptive cards directly under `.agent/kb/`, the normative floor in `.agent/kb/rules/`.
  `coop init` scaffolds that path and un-ignores `**/.agent/kb/` in place of
  `**/.agent/rules/`. **A repo scaffolded by an older coop keeps working, but its rules stay
  where they are** — re-run `coop init` to migrate the `.gitignore` (the retired un-ignore is
  rewritten in place, never left beside its replacement), then
  `git mv .agent/rules .agent/kb/rules` yourself; coop never moves your files.

- **Rules are cards now, with metadata that shows their age.** Each rule carries `name`,
  `description`, `scope`, `sources`, `check`, and `updated`, plus a changelog — the same
  discipline the kb cards already had, so a rule that has drifted from the code it governs is
  visible instead of silently obeyed. `check:` names the command that fails on a violation, or
  `none`, which makes the graduation ladder countable: `grep -l 'check: none'` is the list of
  rules still riding on review. `make rules-check` (new, in `make check`) verifies every card
  parses, is indexed, and doesn't name a `sources:` path or a `check:` command that doesn't
  exist — a gate that can't be run is worse than none.

- **New `/rules-propose` skill** mines the repo's own history for failure shapes that recur,
  requires two independent incidents before proposing anything, and reports separately when a
  rule already covers a cluster — meaning the rule isn't holding and needs a check rather than a
  rewrite. It proposes; a human accepts. Coop-only, alongside `/release`.

## 7.3.0

- **Monorepo members are detected at any depth, and registered for you.** `coop init` only
  looked one directory down, so an umbrella whose members nest — `terraform/environments/va1` in
  an infra repo — could never be detected, and its own help said deeper layouts were "a hand-edit
  of .agent/project.yaml". Worse, even a detected member was only *reported* on a repo that
  already had a project.yaml ("add these to 'subprojects:'"), every init, forever — and an
  unlisted member is a queue coop silently ignores. Detection now walks to any depth (skipping
  hidden dirs, dependency/build output, and anything inside a member), and a newly-found member is
  written into `subprojects:` by a surgical line edit that leaves the file's commented template
  intact. A repo with no members, and an existing umbrella like emisar's five, are both unchanged.

- **`--project` takes a member's last segment when that's unambiguous.** Filing work in a nested
  member meant typing `--project terraform/environments/va1`; `--project va1` was an "unknown
  project" error. The shorthand resolves when exactly one member ends in that segment. With two
  (`apps/web`, `site/web`) it stays an error — silently picking one would file the work in the
  wrong queue — and the message now names both candidates instead of just saying "unknown".

- **Re-running `coop init` is quiet now, and stops interrogating you.** On an
  already-scaffolded repo it printed a prompt ("add sibling services for the box?") whose answer
  could not take effect — every scaffold write is no-clobber — then twenty `kept existing …` lines
  whose entire content was "nothing happened", then first-run advice ("review .agent/Dockerfile,
  then `coop build`") to a repo that had been building for weeks. It also claimed to
  `set core.hooksPath=.githooks` when that was already the value, and announced "scaffolding an
  asdf-driven .agent/Dockerfile" one line before reporting it kept the existing one. A re-init now
  asks nothing, reports what it kept as a single total, and says `already initialized at <repo>` —
  naming the path because `coop init` scaffolds the git ROOT, so run from a subdirectory it acts
  somewhere other than where you're standing. Anything genuinely new (a skill added by a coop
  upgrade) still prints on its own line, where it's now actually visible. `--services` / `--stack`
  are unaffected.

- **`coop check-secrets` stopped calling a secret's NAME a secret.** Two shapes that are
  everywhere in Terraform were flagged as literal credentials: a secret-manager path
  (`GITHUB_TOKEN = "secrets/desktop-release-manager/github-token"`) and a kebab-case resource id
  (`enrollment_secret = "emisar-gcp-runner-tfe-token"`). The absolute-path guard never saw the
  first (the path is relative) and the bare-identifier guard only understood snake_case, not
  kebab. On one real infra repo that was 10 of 12 findings — the ratio that gets a scanner turned
  off, at which point it catches nothing. A value now reads as a NAME when every segment is
  lowercase alphanumeric joined by `-` `_` `.` `/` **and** it contains a letter outside the hex
  alphabet; that second condition is what keeps UUID-shaped credentials firing (Heroku API keys
  are UUIDs). `REPLACE`/`REPLACE_ME` joins the placeholder vocabulary. Same repo now reports 1
  finding — a real committed token the scan was right about.

- **The box image now fits the repo's toolchain instead of imposing one.** The asdf
  `.agent/Dockerfile` was a static file that installed Erlang's build deps, a Postgres client and
  inotify into *every* repo, seeded hex/rebar, and set kerl's build flags — while installing no
  Python. So a Terraform repo carried a stack it never used, and `coop build` failed outright the
  moment `.tool-versions` pinned a tool whose asdf plugin installs through pip (`checkov`,
  `ansible`, `awscli`, …) or that asdf compiles from source (`python`, `ruby`). The Dockerfile is
  now GENERATED from `.tool-versions` the same way the commit gates already were: a universal base
  plus exactly the system packages the pinned tools need. An unknown tool contributes nothing and
  the generated file says where to add its packages. A failing `asdf plugin add` now fails the
  build instead of being swallowed and surfacing later as a missing command.

- **Scaffolded docs no longer teach coop's own repo.** The templates are coop's real working
  files, so its stack leaked into every project: `/investigate` told an agent to reproduce with
  `go test ./pkg -run TestX`, the `.agent/tasks/README.md` worked example (task, `state.md`,
  `log.md` and `decision.md`) was coop's own `COOP_EGRESS` bug in `internal/box/egress.go` verified
  by `make check`, `project.yaml` illustrated context routes with `**/*.ex`, and `AGENTS.md`'s gate
  assumed a compiler (`<build --warnings-as-errors>`). All are now stack-neutral — a Terraform repo
  gets examples it can actually follow.

- **`coop init` no longer commits starter subagents.** It used to scaffold
  `.claude/agents/deep-reasoner.md` and `fast-worker.md` into every repo. A preset already
  generates its own `coop-<role>` subagent in the box, so a repo with its own roles ended up
  carrying two competing sets — and a repo that wanted neither had to delete them after every
  init. Subagents are now yours to write; nothing is scaffolded under `.claude/agents/`.

- **`coop init` scaffolds one copy of the Claude commit gate, not two.** A repo that keeps a
  project `.claude/` adapter also received a byte-identical `.agent/claude/hooks/commit-gate.sh`
  plus a near-identical `settings.json` — files the box never reads, because the project artifact
  always shadows the fallback. Init now writes the project copy when the repo keeps `.claude/`,
  and the `.agent/claude/` fallback only when it doesn't.

- **`coop init` stopped leaving `AGENTS.md` pointing at files that aren't there.** It wrote
  `.agent/tasks/README.md` and then, in the same run, a `.gitignore` block that excluded it — so
  the BOOT protocol's own entry point was missing from every fresh clone. It also cited
  `.agent/rules/static-bounded-supervision.md`, a coop-internal rule no scaffold ever wrote. The
  queue's layout doc is now un-ignored (the task folders around it stay local state), and the
  supervision rule reads inline instead of as a link to a file the repo doesn't have.

- **A re-init no longer duplicates the `.gitignore` block.** Coop probed only for the modern
  `**/.agent/*` spelling, so a repo scaffolded by a pre-monorepo coop got a whole second block
  appended — including a second copy of the `.gemini` stanza it already had. The root-anchored
  rules are now upgraded in place, each newer rule spliced in at its load-bearing position. The
  `.gemini` rules are also written only for a repo that actually keeps a `.gemini/`.

- **An empty skill directory no longer stays empty forever.** `coop init` checked whether a
  skill's *directory* existed, so a half-removed skill — or one whose files a `git clean` took —
  was reported as "kept existing skill" on every run while `.claude/skills`, `.codex/skills`, and
  `.gemini/skills` all pointed at an empty tree. Init now checks for the skill's `SKILL.md` and
  restores what's missing, leaving customized files alone.

- **Work an agent discovers now defaults to the queue instead of the backlog.** The loop work
  prompt, `AGENTS.md`, `coop backlog --help`, and the queue README all stated the rule as "simple
  and ready → `00_todo/`, needs a spec → `xx_backlog/`", but "needs a spec" is self-certifying: an
  agent that just found something can always call it design-shaped, so findings piled up in a
  drawer the loop never reads. The bar for the backlog is now the genuinely large — work one
  iteration couldn't finish, or a spec-sized idea a human must scope first — and a close call goes
  to `00_todo/`, where the loop picks it up.

- **Trusted local services can now hold persistent conversations in isolated Coop forks.**
  `coop sessions serve` exposes a strict versioned JSON API over an owner-only Unix socket, with
  durable idempotent operations, one bounded FIFO per session, exact native ACP resume, per-turn
  credential/MCP projection and cleanup, cancellation/restart reconciliation, typed binary-safe
  changes, read-only current-parent review evidence, non-destructive close, and exact-state
  two-step discard. Requests select only operator-owned execution policies; the API has no TCP,
  raw host paths, arbitrary commands, merge, signing, push, PR, Slack, or incident authority.
  `coop sessions doctor` checks the socket, and the external responder design keeps Slack,
  authorization, Emisar approvals, audit delivery, and GitHub publication in a separate service.

## 7.2.0

- **Silent provider attempts are now bounded by a stream-fed watchdog.** Every built-in loop,
  review, and pre-flight attempt requests its provider's structured stream — redirected runs
  included — and only adapter-recognized events count as progress: 10 minutes to the first model
  action, 30 minutes of post-progress silence, and a 2-hour cap on the oldest open foreground
  tool, which suspends the idle deadline so a long gate survives. A fired deadline cancels the
  box, records an explicit `provider_*_timeout` outcome, restores any premature completion, keeps
  held audit authority truthful (rebase a valid complete rewrite, park fail-closed otherwise),
  and retries on the next usable rung without cooling under a dedicated consecutive cap of three;
  a user interrupt still wins over any watchdog fire. Interactive and redirected loops now treat
  SIGTERM/SIGHUP as immediate hard cancellation (while preserving SIGINT's soft-then-hard UX),
  and force-remove the exact run-labeled container if killing its runtime client leaves the
  daemon-owned box behind.

- **Audit-reopened tasks now protect complete descendant history.** Host authority records the
  exact baseline HEAD and every ordered later commit, including manual and release commits without
  a `Coop-Task` trailer. Rework rejects dropped, changed, reordered, or invented unbound history
  while still allowing exact replay and a later host suffix. Legacy task-only v1/v2 records cannot
  lease or complete until a human restores the audited HEAD and explicitly adopts its full SHA;
  blocked recovery for current records uses the persisted baseline instead of a reflog guess.

- **Byte-identical review wrapper echoes count as one verdict.** The host verdict boundary now
  collapses one exact normalized duplicate of the complete evidence-plus-receipt envelope, so a
  Codex usage footer routed separately from its repeated final message does not consume the
  malformed-receipt correction. Conflicting or merely similar records, partial or additional
  echoes, missing or extra subjects, and out-of-scope reopens remain invalid.

- **Deleting a task now deletes its stale orchestration authority too.** `coop tasks rm`,
  `rm --all-done`, and interactive decision deletion remove the task id from interrupted
  completion-window journals, lease receipts, controller metadata, audit-reopen generations, and
  trusted departure records before deleting its folder. A killed run can no longer make a
  deliberately cancelled task reappear on the next loop start. Removal refuses a still-live lease
  and leaves the folder intact until that controller stops.

- **Loop and review boxes now preserve live background work long enough to hand it off safely.**
  Their entrypoint distinguishes authenticated service forwarders from agent-owned descendants,
  including detached sessions; it drains or terminates the latter on a bounded deadline and reports
  an entrypoint-owned status that providers cannot forge. Coop restores any premature task
  completion, discards an unobserved review receipt, records the handoff, and starts a fresh
  provider attempt. Three consecutive handoffs stop with an actionable foreground-work message;
  interactive boxes keep their existing immediate-exec behavior.

- **Host-receipted foreign reopens no longer kill a work iteration.** A work completion window now journals its exact assigned task and accepts a concurrent host `coop tasks claim` of a different archived task only when a host-only, nonce-bound departure record proves the move invalidated that archive's baseline completion receipt. Raw folder moves, stale or forged records, duplicate subjects, and any change to the assigned task still fail closed; crash replay applies the identical classifier.

- **The loop announces and pins its `loop.yaml` snapshot.** Every loop — including a fork loop —
  now logs the exact startup `.agent/loop.yaml` it runs on (a short SHA-256 digest, or an explicit
  absent/built-in-defaults state) and derives ladders, prompts, round caps, and write policy from
  that one read for the whole run. A mid-run edit no longer silently never applies: before each
  later work or review box launches, Coop compares the on-disk bytes with the snapshot and prints
  one actionable `restart to apply` warning per new digest, while the running orchestration stays
  on its coherent startup configuration.

- **Externally blocked audit rework keeps its one-time verification authority.** When a
  review-reopened task rewrites an older implementation while replaying its later task commits,
  then pauses in `50_blocked/` for credentialed or host-only acceptance, Coop now validates that
  rewrite and rebases the same host-owned generation to its new semantic subject. After a genuine
  unblock, the task can close verification-only without another history rewrite; changed
  descendants, replaced generations, task-local forgery, and reuse after completion still fail
  closed.

- **An annotated `none` in audit evidence no longer voids a passing review.** The findings field
  now accepts exactly `none` or `none (parenthesized annotation)` — the form models habitually
  write — so a PASS receipt with `findings: none (gate green, no scope creep)` applies cleanly
  instead of dying as a malformed verdict. The injected evidence contract states the same grammar.
  Anything looser ("none — flaky test fails", "none-critical issues found", invalid UTF-8) still
  reads as a finding, keeping the receipt/evidence agreement check fail-closed.

- **A malformed review receipt gets one bounded correction turn.** Between, signoff, and verify
  now re-run the complete review once over the same subjects under the same configured writes
  policy when a successful process returns malformed structured output, appending a fixed
  receipt-format reminder and recording both attempts. Valid recovered PASS/FAIL proposals apply
  once; a malformed second proposal,
  lifecycle churn, interruption, or provider failure retains the existing fail-closed behavior.
  Codex review normalization also accepts its observed footer/count plus one byte-identical echo
  of the complete explanatory response, while non-identical or additional proposals stay invalid.

- **`coop up` names the Compose services it actually starts.** The success hint now gets the
  exact non-empty service names, in Compose order, from the selected runtime's resolved
  configuration instead of inventing `db`, `redis`, or another example. A discovery failure
  stops before `compose up` and never prints a misleading success line.

- **Claude model-credit limits rotate the loop instead of spending failure strikes.** A terminal
  `Fable` limit reply that points to `/usage-credits` and `/model` now advances to the next account
  and then the next provider, whether Claude exposes structured rate-limit metadata or prints only
  the notice in a failed non-streaming run. If every target is limited, the loop uses its bounded
  wait and resumes on the first recovered rung; ordinary assistant prose about limits remains
  non-authoritative.

- **Review completion windows are scoped to their exact subjects.** A parallel host session
  finishing an unrelated task (`coop tasks done`) while a between/signoff/verify box runs no
  longer kills the loop with "review completion set changed". The window records the review's
  subject ids in its crash journal: subject churn, unreceipted folder moves, reopens, and
  deletions still fail closed, while a host-receipted foreign completion is tolerated, reported,
  and excluded from the re-anchored signoff baseline so it joins the next review round instead
  of being silently absorbed. A final verify that observes one returns to signoff before exit,
  and crash replay preserves that pending review.

- **Review task ownership is host-enforced.** Task-scoped review boxes now see the whole
  repository read-only and return bounded evidence plus a proposed verdict. Coop validates the
  complete proposal and applies all exact-subject reopens atomically under host authority;
  malformed, interrupted, failed, and out-of-scope reviews cannot mutate task lifecycle state.
  Even `writes: repo` remounts every task queue read-only. Reviewer findings are stored only as
  delimited untrusted evidence; the next worker receives a fixed reproduction-first action.
  A host-authenticated, single-use reopen generation now also permits a false finding to be
  re-closed without a receipt-only commit, or an older implementation commit to be repaired while
  exact later task changes are replayed unchanged. Changed, invented, duplicated, redirected, and
  reused bindings still fail closed, including across interrupted completion cleanup.

- **A task has exactly one reachable commit binding.** Completion rejects both a missing
  iteration binding, any second `Coop-Task` trailer reachable from `HEAD`, and any binding for a
  different task in the iteration range. Reopened work must amend or rewrite its original task
  commit instead of stacking another bound commit, and verify authority comes only from
  host-recorded completions. Ordinary completion derives both checks from the raw commit-object
  DAG, so provider-writable graft and shallow metadata cannot hide, invent, or redirect bindings.

- **Signing no longer depends on a clean active checkout.** Coop re-signs in an isolated linked
  worktree, verifies that commit trees are unchanged, then updates the original branch with an
  old-SHA compare-and-swap. Staged, unstaged, untracked, and secret-decoy files remain untouched,
  side refs cannot be rewritten, repository-local SSH key commands are ignored, and a concurrent
  ref move leaves the signed candidate unapplied.

- **Agent-supervised long commands keep static, bounded output.** The canonical and scaffolded
  agent instructions now require repainting controls, redirected complete logs, preserved exit
  status, and bounded tail or filter inspection, without degrading Coop's human-facing live UI.

## 7.1.0

- **Compose sidecars now reconcile removed services and basename-era projects.** `coop up`,
  automatic box startup, and `coop down` pass `--remove-orphans`, so changing the configured
  Compose file no longer leaves old containers running. Coop also retires the pre-workspace-hash
  project only when every matching container's Compose labels prove it belongs to the current
  checkout. Legacy volumes are preserved because they carry no safe checkout ownership label.

- **Projects can declare committed box-only environment defaults.** Add literal strings under
  `box.env` in `.agent/project.yaml`; the user's `agents/env` and Coop's generated runtime values
  still win, and the reserved `COOP_*` namespace is rejected. Every box now receives the stable
  `COOP_BOX=1` identity marker. Loop boxes request configured dev-server publishing, and retain
  their assigned `COOP_SERVE_URL_<port>` for configuration even when an existing listener means
  that particular box cannot publish the port.

## 7.0.0

- **The box image and sidecar compose paths are configurable, and the box Dockerfile now lives at
  `.agent/Dockerfile`.** Set `box.dockerfile` / `box.compose` in `.agent/project.yaml` to point at
  any in-repo file (reuse an existing `Dockerfile`, or your own `docker-compose.yml`); they default
  to `.agent/Dockerfile` (moved from the repo-root `Dockerfile.agent`) and `.agent/compose.yml`.
  `coop init` scaffolds and git-tracks `.agent/Dockerfile`.

- **A project box Dockerfile can inherit coop's base image.** Write `ARG COOP_BASE_IMAGE` /
  `FROM ${COOP_BASE_IMAGE}`, and `coop build` resolves the shared base (building it first if
  missing), passing it in as a build-arg — so your box gets coop's agent CLIs, browser libraries,
  and security setup, and you add only your own toolchain.

- **`coop context` compiles the committed docs relevant to the paths in play.** A new
  `context.routes` map in `.agent/project.yaml` maps path globs (`*` within a segment, `**` across)
  to instruction/rule/KB docs; `coop context [--changed] [--task <id>] [paths…]` (plus `--json` and
  `--rendered`) selects the canonical `AGENTS.md`/`CLAUDE.md` (always, whole) plus every route whose
  glob matches a deterministic scope path — explicit paths, git-changed paths, a task's declared
  `paths:`, or the current subproject, never inferred from a prompt — deduped, reported with the
  route that chose each. A missing or repo-escaping include is rejected.

- **Forks get their own serve ports, discoverable as JSON.** A fork inherits its parent's
  `serve.ports` but now allocates each host port from its OWN workspace path (not the shared policy
  repo), so parallel forks and the root never collide on one host port. Each box also gets a
  `COOP_SERVE_URL_<port>` env var — its host-facing URL, e.g. for an OIDC redirect — and
  `coop fork ls --json` lists every workspace (root + forks) with its serve URLs, so host tooling
  discovers them without reproducing coop's port hash.

- **Sidecars are per-workspace, reachable at one URL inside and out.** The compose project + network
  names now derive from the workspace path, so a fork's services and volumes are its own (not shared
  with the parent or another clone). An `expose:`d port in `.agent/compose.yml` is published to a
  stable per-workspace loopback host port, and a raw-TCP forwarder in the box (baked-in `socat`,
  started by the entrypoint from `COOP_FORWARD`) makes that same `localhost:<port>` reach the sidecar
  from *inside* the box — so an OIDC issuer matches on both sides, no `host.docker.internal`, no
  weakened isolation. The host port is keyed on service *and* port, so two sidecars — or a sidecar
  and a `serve.port` — never collide. The box gets `COOP_SERVICE_<NAME>_URL` (scheme from an optional
  `coop.service.scheme: https` compose label, default `http`), and `coop fork ls --json` reports each
  workspace's service URLs. Config (the compose path) is read from the policy repo, the file from the
  workspace. (The box image gains `socat`; run `coop build`/`coop update` to rebuild.)

- **`coop tasks --blocked` works without the `ls`.** A bare leading flag on `coop tasks` now
  routes to the listing — `coop tasks --blocked` is `coop tasks ls --blocked` — instead of failing
  with an "unknown subcommand" / "works one queue at a time" error, in a single repo and umbrella
  projects alike.

## 6.3.0

- **`coop tasks ls` filters by state, and task ids are clickable.** Pass one or more of
  `--todo` / `--in-progress` / `--blocked` / `--done` to narrow the listing (and its footer
  summary) to those states — across a single queue or an umbrella roll-up. Every task id is now
  an OSC 8 hyperlink to its folder, so it opens on click in a supporting terminal (plain text in a
  pipe), pointing at the right per-subproject path in umbrella projects.

## 6.2.0

- **The ACP preset dropdown now previews every preset on hover.** Each preset option — not just
  the active one — shows its declared lead ladder and subagent roster in the help text, so you can
  read what a preset does before switching to it. The active preset still leads with its live rung.

## 6.1.0

- **The ACP preset dropdown now shows its full subagent roster.** The selected preset's help
  lists each role beneath the active lead target — name, read-only/writes mode, its
  `provider:model/effort` target, and its routing hints — so the whole orchestration recipe is
  legible in the editor without opening the preset YAML.

## 6.0.0

- **Scaffolds and adopted repos keep their existing conventions working.** The asdf Dockerfile
  template keeps the toolchain on the box login path, monorepo `coop init` prints each member's own
  scaffold paths, established `.agent/skills` links are never repointed away from their source, and
  a repo-local `core.hooksPath` chains Coop's box attribution hook instead of silently disabling it.
  Gate-defining-change review also detects queue-guard scripts under adopted queues.

- **In-progress tasks are leased to their loop controller.** `coop tasks list` and `watch` label
  leased work busy, stalled, or unleased; a free lease is adopted immediately, and a stale heartbeat
  under a held lease reads as stalled rather than being stolen.

- **ACP provider switches keep internal carry context out of the editor.** Coop now removes its
  exact injected history from provider user echoes, Codex's whitespace-normalized session titles,
  and Grok's prompt-queue notifications while preserving surrounding text and metadata. The live
  process wrapper also preserves the editor's stdin across its supervised background launch.
  Strict conformance follows each adapter's advertised model capability instead of failing a
  single-model provider or inventing unsupported dropdown choices.

- **Provider conformance now stays aligned with the four-adapter registry.** Fork ACP completion and
  missing-provider guidance include Grok, generated source-tree docs name the deterministic and live
  gates, and refresh-only Claude credentials are stripped of refresh authority then reported as
  `credential_refresh_required` instead of an unsafe projection failure. Live loop verification now
  preserves failed/skipped provider outcomes against an unchanged fixture and accepts descriptive
  commit bodies while retaining exact subject, trailer, repository, index, ref, and reflog checks.
  `coop credentials` now distinguishes refreshable OAuth sessions from malformed, stripped, or
  expired Claude/Grok markers and gives the latter one exact re-login command.

- **Provider session lookup is bounded and tested against large native histories.** A generated
  all-provider matrix now covers exact resume, full misses, wrong cwd and ID, malformed entries,
  account isolation, descriptor closure, and diagnostic allocation/latency benchmarks. Gemini skips
  current foreign buckets by their bounded `.project_root` marker and limits legacy whole-record
  decoding to 4 MiB. A separate opt-in live target creates a session in one clean
  helper process and resumes its exact native ID in a second, proving marker continuity while
  retaining the existing read-only repository, access-only credential, source-integrity, and
  process-cleanup boundary. Supervision and ACP transcript sharing are now separate box settings,
  so a supervised non-ACP probe no longer shadows its provider profile's native session history.

- **Detached forks and fleets now have crash-safe, repository-scoped cleanup.** Runtime boxes keep
  a readable fork label but are reaped by a repo-scoped owner, so two repositories may use the same
  fork name safely. Unverified and reused PID records report `cleanup` instead of running, missing
  workspaces remain stoppable, stop/down are idempotent, and removal/prune re-check lifecycle state
  under the fork lock after confirmation. `--fresh` now uses the same confirmation and locked
  recheck, fleet prune requires `--yes` separately from `--force`, and upgraded legacy state stays
  visible when its old container label cannot be reaped safely. Deterministic external-process
  coverage drives all four providers, preset rotation,
  duplicate starts, status/watch, worker crash, exact-label reap, reused PID refusal, cross-repo
  isolation, confirmation, and complete process/state cleanup.

- **Fork re-entry now keeps provider sessions isolated by account and workspace.** Claude,
  Gemini, and Grok persist one explicit session ID per fork/provider/account; Coop records the
  native ID Codex mints and resumes that exact interactive session. `--new` rotates the
  explicit ID, `--fresh` preserves the remembered provider before recreating the clone, and a
  provider switch starts a separate native session rather than carrying a transcript in place.
  Shared `COOP_WORKDIR` paths fail fresh rather than guessing when an old Codex fork has no hint.
  Provider-writable fork hints are bounded and no-follow, merge reconciliation considers only the
  commits just landed, and deterministic external-process coverage now proves fresh, resume, new,
  provider/account switching, seeded task work, confirmation, merge, and parent queue settlement.
  Coop-owned interactive Codex producers for one account/workdir now share a config-global,
  fail-fast lock so concurrent repos cannot cross-claim a native session. A fork-loop target that
  specifies only effort now preserves that explicit choice as its one-off ladder entry.

- **Loop review stages are now process-tested as one closed provider workflow.** Deterministic
  coverage drives work, between-task audit, signoff, and verify on distinct provider/model/account
  targets; checks the real read-only-review mount policy; and covers review rotation, reopens,
  retry exhaustion, the signoff round cap, soft-stop auditing, native usage attribution, and final
  queue/exit bookkeeping. Review telemetry now preserves the terminal outcome, effective target,
  usage, and total retries instead of hard-coding success. A verify reopen exits nonzero, stale
  reset hints use bounded backoff, oversized nonterminal stream events no longer invalidate a later
  terminal success, and every unleased task moved to done during an iteration is restored. A
  host-only completion receipt distinguishes a released foreign controller without trusting
  provider-writable task metadata, while a crash-durable done-folder fingerprint journal extends
  the check across controller death and every writable review stage without reclassifying
  historical completions made outside a supervised box. Receipt invalidation is generation-bound,
  work stages reject raw archive reopens, and reviewers never mutate archived task metadata in
  place after host finalization.

- **Loop recovery policy is now process-tested across every provider transition.** The external
  deterministic matrix covers authentication, provider/output limits, ordinary failures,
  malformed and truncated streams, account/model cooling, interruption and resume, all 12 directed
  rotations, and cancellation from the all-limited wait state. Recovery now validates commit
  binding before finalization, returns crash-left completions to range-validated resume, bounds
  streamed events and stderr lines, distinguishes one terminal classification/telemetry outcome,
  keeps the authoritative task flock and active marker in host-only state, stops heartbeats before
  cleanup, finalizes only the assigned task, rejects ambiguous non-archived IDs across queues,
  publishes telemetry and traces without following links, and bounds telemetry reads. The existing
  opt-in all-provider task probe remains the upstream CLI compatibility gate; deterministic tests
  own recovery policy so live checks never retry or induce quota exhaustion.

- **Provider loop coverage now crosses the complete task lifecycle for every adapter.** The
  deterministic external-process matrix proves claim and lease ownership, native provider argv,
  task state, exact `Coop-Task` binding, done reconciliation, cleanup, and effective telemetry for
  Claude, Codex, Gemini, and Grok. A separate opt-in live target gives each selected provider one
  writable model attempt to complete an exact disposable task under the existing isolated
  credential, deadline, and cleanup contract. Rejected unbound completions now return to
  in-progress with a concrete recovery state instead of retaining a stale complete snapshot;
  recovery metadata reads and appends refuse provider-created symlinks, hardlinks, and special files,
  and failed completion finalization restores the task to the actionable queue before returning.

- **Box-generated Git mounts no longer make `~/.config` root-owned.** The co-author hook and global
  ignore file now mount at direct home children, preserving Git behavior while allowing Chromium,
  Playwright, and other tools to write their application config as the box's non-root user. Existing
  stock `.githooks/prepare-commit-msg` shims are migrated by running `coop init` once; custom hooks
  remain untouched.

- **Credential discovery now follows each adapter's complete authentication contract.** Claude's
  alternate auth/OAuth tokens and Gemini's `GOOGLE_API_KEY` now drive credential listing, peer and
  Fusion availability, loop/fleet ladders, ACP defaults, and scoped role mounts just like primary
  keys or file-backed logins. An env token renders as one usable default credential rather than a
  false pool of every named account, marker-backed accounts (including a marked default) cannot be
  shadowed by it, and bare imports use runtime-compatible duplicate precedence.

- **Fusion councils now resolve once and stay valid across terminal and ACP runs.** Terminal
  presets pin and report their first lead rung instead of rejecting cross-provider ladders; ACP
  retains the full ladder, filters the active governor from explicit peers on every spawn, and
  rejects any preset whose council would become empty on a reachable provider. Explicit peers are
  unique by provider, preset members are distinct by role name (so several roles may share one
  provider), provider/role invocation collisions fail early, and role-only presets now mount the
  actual `coop-consult` wrapper their contract names. Instructions require every one-or-many
  council member without assuming exactly two. ACP also carries an explicit empty preset selection
  into the child, hides/refuses None when it would create solo Fusion, and skips unused plain-box
  prewarming. Preset first-account pins use the normal credential validator, and role defaults no
  longer inherit an unrelated raw peer's model.

- **Loop runs can be bounded with `--max-tasks N`.** A bounded run counts a task only after
  done/blocked settlement following retries and its immediate audit, then pauses before another
  claim or final signoff — exit code 0 marks the intentional pause. Unlimited loops drain normally.

- **Non-zero progress never rounds to invisible.** In-progress work keeps at least one yellow cell
  in loop, fleet, and task-watch progress bars, while blocked work retains its existing
  one-red-cell minimum.

- **Loop status stays on the work actually running.** Sticky bars keep their assigned task through
  folder moves, while between, protected, signoff, and verify passes name their own stage and review
  subject instead of drifting to the next queue item. Provider closing lines now label compact token
  totals as `input` and `output`, including the website cast.

- **Completed task snapshots are deterministic.** Every direct, loop, retry, and fork-reconciled
  completion atomically sets `state.md` to `Status: complete` and `Next action: none` before review,
  preserving useful summaries and traps before removing task-local `tmp/`. Reviewers do not mutate
  archived tasks in place; every reopen receives a concrete resume state.

- **Zsh no longer second-guesses valid Coop targets.** Sourcing the generated completion after
  `compinit` installs command-local `nocorrect` behavior for `coop`, preserving dynamic completion
  while leaving spelling correction enabled for every other command. Existing file-only installs
  need the new source line documented by `coop help completion`.

- **Peer consult replies and continuity now fail coherently.** Generated lead guidance keeps replies
  separate from diagnostics and requires polling yielded sessions through terminal exit. Usable
  replies atomically publish one bounded continuation record; failed resumes preserve the last
  complete transcript for the next call, while empty, malformed, timed-out, oversized, and
  provider-failed attempts stay terminal without publishing false continuity or telemetry.

- **A consult role with no mounted target rejects cleanly in the box.** The wrapper left an
  internal variable unset until the first admitted target, so under the box's dash `/bin/sh` a
  role whose every target lacked mounted credentials aborted with a shell error instead of the
  intended "no target with mounted credentials" message. It now initializes up front, matching the
  delegate wrapper.

- **Final signoff receives bounded between-audit evidence.** Receipt-consistent ordinary and
  protected audits retain a compact per-task verdict, tested gate, and unresolved findings for the
  same loop run. Signoff treats reviewer-reported details as context, not acceptance, and stays
  independently authoritative; review stages now also prohibit recursively invoking `review-board`
  while permitting focused read-only investigation.

- **Box Run and Corner Run are Coop's signature spinners.** A five-column Box Run animates beside
  progress bars and a one-column Corner Run (`◰ ◳ ◲ ◱`) marks dense task rows; every live surface
  stays aligned, and `COOP_SPINNER=0` keeps debug and recording captures quiet.

- **ACP presets now exclusively own lead selection.** A normal toolbar shows Preset, Provider,
  and Account; an active preset shows only Preset because its ladder owns provider, model, effort,
  account, and roles. Persisted hidden Provider/Account sets are acknowledged as no-ops, and
  selecting None returns the effective provider with Account set to Auto without restoring old
  plain values.

- **ACP provider switches keep editor threads stable without crossing native session IDs.** Coop
  now persists each editor/native/provider identity, loads native history only on the provider that
  owns it, and creates directly on another provider. Failed load/prompt responses no longer create
  phantom replay state; close deactivates a thread while retaining its resumable identity, delete
  removes its identity and carry caches, and stale output from a retired child is dropped before it
  can mutate or duplicate the live thread.

- **Review receipts are bound to exact task deltas.** Between and signoff reviews now report an
  explicit PASS/FAIL verdict plus a deterministic task-ID list. Coop compares those IDs with the
  review's done-to-actionable folder delta and named subjects, so unrelated actionable work cannot
  distort health, telemetry, round outcomes, or receipt validation; malformed and count-only legacy
  receipts fail closed. Per-commit protected-file attribution now reads NUL-delimited Git paths, so
  non-ASCII directories cannot hide gate edits from a task-specific review. Aggregate affected-area
  names use the same exact-path handling instead of Git's quoted display form.

## 5.3.0

- **Gate-defining changes earn an immediate, fail-closed review.** A completed loop task that edits
  the gate, loop/project configuration, hooks, sweep enforcement, or CI is audited before another
  task can trust that checker, even when ordinary between-task review is disabled. Exact paths and
  final signed HEADs remain in run telemetry, while final signoff independently derives protected
  paths from each task's committed diff.

- **Fresh scaffolds scope queue completion enforcement to `/sweep`.** New projects omit the
  project-global Claude Stop hook and `.agent/active` sentinel, so loops and unrelated sessions are
  never held by stale sweep state. The sweep skill's own hook blocks while either todo or
  in-progress tasks remain across every configured or mounted queue. Existing projects are kept
  non-destructively; use the README migration steps to retire their stock global hook safely.

- **Task lifecycle changes stand out in the loop live view.** In-box folder moves now read as
  claim, queue, done, block, or unblock events; task/log/state/decision edits name the task instead
  of showing a long mount path, and setup-only task-directory creation recedes to a short line.

- **Loop orchestration reads as orchestration.** Read-only consults, write-capable delegates, and
  native Claude subagents now have distinct glyphs and role/type labels in the live view instead of
  masquerading as generic Bash or `Task` calls; failures keep the same semantic label.

- **Loop Bash activity keeps the useful part of repo paths visible.** Repeated in-box repo mount
  prefixes are stripped from streamed Bash command lines before truncation, while absolute paths
  outside the repo remain untouched.

- **Fork review can prove a rebase and gate before merge.** `coop fork review <name> --gate`
  rebases in a disposable scratch clone, runs the parent's configured gate in the box with the
  candidate mounted read-only, and reports green, red, or conflict. The parent and fork stay
  untouched, red/conflict exits non-zero, and the scratch clone is removed on every path.

- **Fork names are shell-inert by construction.** The accepted grammar is now the small ASCII set
  used by normal branch segments (letters, digits, `.`, `_`, and `-`), so shell metacharacters are
  rejected before any workspace, subprocess, or runtime operation. Every execution boundary still
  passes the name as argv or environment data rather than interpolating it into shell source.

- **A Claude box gets settings and hooks from the `.agent/` cornerstone.** `coop init` now keeps
  fallback Claude settings and stack-aware hooks under `.agent/claude/`. When a repo omits the
  matching project `.claude/` artifact, Coop mounts an ephemeral user-level copy for that run;
  project artifacts still win independently and the committed fallback stays pristine.

- **Loop iterations now receive an authoritative task assignment.** The host selects interrupted
  work first, otherwise claims the next todo before starting the box, and gives the agent that exact
  task ID. The banner and prompt therefore cannot diverge, and agents no longer spend a turn
  guessing or moving the queue's claim themselves.

- **Streaming loop traces preserve both sides of the live view.** Set `COOP_STREAM_TRACE=1` to
  capture every streaming box attempt's byte-exact provider JSONL and Coop's rendered lines under
  `.agent/runs/<run>.streams/` with owner-only permissions. Tracing is best-effort and disables
  itself after one file-open warning, so a full or unwritable disk cannot break the loop it is
  observing.

## 5.2.0

- **Every provider streams the same live view in the loop.** Codex dumped its banner and raw exec
  blocks, Gemini printed only errors, and Grok buffered run-on prose; each provider now enables its
  streaming JSON mode on a TTY and decodes it into the same model line, tool glyphs, `✦` text, and
  closing token line. Work and review stages led by Codex, Gemini, or Grok also record their token
  usage in the run's cost telemetry; dollars remain Claude-only because the other CLIs do not
  report cost.

- **Zsh completion now follows Coop's full target grammar.** The first Tab after an exact command
  advances to that command's arguments, and target positions complete available providers, models,
  effort levels, credentials, and presets. Loop, fusion, and ACP peer positions also offer the
  contextually valid targets and flags instead of falling back to the top-level command list.

- **A gemini box can read the gitignored task queue.** Gemini's `read_file` rejected every
  `task.md` as "ignored by configured ignore patterns": its file tools honor `.gitignore`, while
  the queue is gitignored working state. Coop's mounted per-box settings now force
  `context.fileFiltering.respectGitIgnore=false` without changing the host settings.

- **The pre-flight prompt no longer contradicts a queue-managing cleanup.** The built-in unblock
  already runs host-side, but the custom-cleanup box still forbade queue moves; one agent wrongly
  responded by parking five healthy tasks in `50_blocked/`. Its wrapper now permits only directed
  folder moves while task work, code, gates, and commits remain forbidden.

- **Long loops stop repeating and immediately retrying known rate limits.** A provider's structured
  limit notice is rendered once while its full text still reaches the detector, and a failed custom
  pre-flight now carries the target's reset into work rotation. Cooling targets are skipped until
  their reset; when every target is limited, the loop still waits for the earliest one.

- **Loop boxes now enforce the same Staticcheck gate as the host.** The shared image builds and
  installs a pinned analyzer, closing the gap that let a host-red commit pass inside a loop. The
  fork hardening fixture also neutralizes ambient global Git hooks repo-locally, so the stronger
  in-box gate remains deterministic.

- **`coop fork stop` now proves the fork's box is gone before reporting success.** The worker still
  gets its graceful process-group shutdown first; the exact `coop.fork=<name>` container reap now
  has a deadline and surfaces runtime query/removal failures instead of confusing them with an
  already-gone box. Failed reaps and crashed-worker state stay retryable, and a per-fork lifecycle
  lock prevents a new start from racing that cleanup; `fleet down` also propagates any stop
  failures. Apple `container` uses its native JSON listing while Docker/Podman retain label filters,
  so the backstop is exact across the supported runtimes.

## 5.1.0

- **The loop reports what a run cost — per task, per model, and everywhere you review it.** The
  closing digest now shows cost + tokens per shipped task and a run total; a run that spread work
  across models (a preset's lead, its review stages, and consult peers) gets a `by model:` line —
  and each iteration's line gains its token count. The same tally surfaces on `coop fork review` and
  `coop fork merge` (before you land), on the `coop fleet watch` board (per fork + a fleet total),
  and as a COST column in `coop fork ls`. It all reads the run telemetry each loop already writes
  (`.agent/runs/`), so there's no new bookkeeping. Cost is captured for claude-led runs (its result
  event carries it); codex consult peers contribute their tokens (codex's stream reports no cost).

- **`coop check-secrets` precision: minified bundles stop entropy-firing, JWTs get caught.** The
  entropy heuristic now skips a match drowning in a minified/generated line — one with over 2 KB
  of code *around* the assignment (a bundle puts a whole program on one line, where a high-entropy
  `token:"…"` literal is a build artifact). Keying on the surrounding slack rather than line
  length keeps the real cases firing: a secret pasted next to minified code sits on its own line,
  a multi-KB base64 credential blob is all value, and the precise provider patterns scan every
  line regardless. A committed 3-part JWT (`eyJ….eyJ….sig`) is now reported by its own provider
  pattern — its dotted shape used to parse as a code reference and get suppressed. UUID-shaped
  values deliberately keep firing: real credentials are canonical UUIDs (Heroku API keys), so
  exempting that shape would blank a live credential class.

- **Simple discovered work goes to the queue, not the backlog.** The backlog is now scoped to the
  BIG or not-yet-ready — work that needs a spec, a decision, or real scoping first. A simple, ready
  task an agent spots while working goes straight to `00_todo/` (`coop tasks add`) so the loop
  drains it, rather than into a drawer nothing auto-touches (the reason it had bloated to 21 items
  the human then hand-triaged back). The scaffolded contract, the loop work prompt, and `coop
  backlog` help all say so, keeping the backlog a short list of things that genuinely need planning.

- **The loop's closing digest nudges you to prune a piled-up `99_done/`.** Done tasks are removed
  only by a human, so once ten or more have accumulated the loop closes with a one-line reminder to
  run `coop tasks rm --all-done` after you review and push — it never prunes anything itself.

- **`loop.yaml mcp: false` / `coop loop --no-mcp` — run a loop without the shared MCP config.** MCP
  tool schemas ride at the front of every model request, so an unattended drain that never touches
  those tools pays for them each iteration, every stage, all run long. `mcp: false` (committed) or
  `--no-mcp` (one-off) keeps `~/.coop/mcp.json` out of every loop box: no `--mcp-config` for
  claude, no generated codex/gemini configs. Leave it on if a `verify:` pass depends on MCP tooling
  (repo-local e2e via bash is unaffected).

- **The loop's built-in preflight runs host-side — no box, no model, no tokens.** Unblocking a task
  whose decision.md gained a `**Resolution:**` is mechanical, so coop now does it directly (the
  same bar as `coop tasks unblock`; a decision format it can't read stays parked). An agent box
  spins up only for a custom `preflight.prompt`, which now SETS that extra cleanup pass instead of
  appending to a built-in prompt.

- **Loop prompts stop re-reading the contract.** Every agent auto-loads its instruction file (the
  `CLAUDE.md`→`AGENTS.md` symlink / `AGENTS.md` / `GEMINI.md`), yet the work and preflight prompts
  opened with an unconditional "Read AGENTS.md" — a duplicate ~2K tokens plus a wasted tool turn,
  every iteration and every review pass. The prompts now reference the contract and offer the
  absolute path only as a fallback when it isn't in context.

- **The signoff reviews what the run completed — not all of `99_done/`.** Each signoff round now
  gets an explicit subject list: the tasks that entered `99_done/` since the last accepted round
  (for round 1, since the run started). Prior runs' history — `99_done/` persists until you prune
  it — and tasks an earlier round already accepted are no longer re-reviewed every round, which was
  the loop's single biggest token burner; a reopened task's rework still re-enters the next round's
  subjects. A run that completed nothing skips the signoff pass entirely.

- **The loop hands its review stages what changed — plus a new `verify:` stage and a closing digest.**
  Every review pass (`between`/`signoff`, and a new opt-in `verify:` pass that runs after signoff
  accepts the batch) now receives the run's CHANGE CONTEXT: each task the loop completed, grouped by
  its `Coop-Task` trailer, with the files it touched and any risk the run flagged (a reopened task, a
  gate-file edit). So a prompt like "e2e-test the affected features" resolves against a concrete
  per-task list instead of guessing — place the context inline with `{loop.changes}` / `{loop.tasks}`
  / `{loop.affected}`, or let `verify.prompt` say it. The loop also closes with a human digest — what
  shipped (per task + areas), what's blocked on a decision, and which tasks to look at.

## 5.0.0

- **One grammar names who runs — and `--preset`/`--consult` are gone.** Everywhere coop names who
  runs (the positional of `coop <agent>`/`loop`/`fork`/`acp`/`fusion`, and the `agent:` key in
  `fleet.yaml` + `loop.yaml`), a bare provider is a TARGET and any other bare word is a PRESET name —
  so `coop loop frontier`, `coop fork feat frontier`, and `agent: frontier` all mean the same thing,
  like a `loop.yaml` ladder rung already did. **BREAKING:** `--preset` is removed (name the preset in
  the positional slot); `--consult` is renamed to `--peer` everywhere (coop already calls these
  agents "peers"); fleet's separate `preset:` key is dropped (`agent:` absorbs it). The retired
  spellings are now ordinary unknown args — update any editor/CI invocation that passed them.

- **Every legacy tombstone is dropped — no retired-form handling anywhere.** coop kept a helpful
  error for every renamed/retired form (old commands, `--model`/`--credential`, retired preset/fleet
  keys, the `.agent/loop/*.md` + `TASKS.md` + pre-v3 `.agent/fleet` files, the flat-vault migration,
  the `.gitignore` in-place upgrade, `--supervise`, the `coop_setup` ACP dropdown). All gone: a
  retired form now hits the ordinary unknown-command/field/argument error or is simply ignored.
  **BREAKING** for anyone still on those forms (MIGRATING.md still documents the moves); notably
  `coop acp --supervise` now errors, so drop it from editor configs.

- **The knowledge base is self-improving — agents own it, no human gate.** `.agent/kb` was
  human-curated: an agent could only drop a draft in `kb/inbox/` for a human to promote. Now agents
  maintain the live kb directly, in the same commit as the work; every card carries an `updated`
  date, its `subsystem`, and the `sources` it maps, plus a small changelog, so staleness is obvious
  — and cards still load only for their own subsystem (the safety rail). `kb/inbox/` is removed.

- **The loop cites a commit by its Coop-Task trailer, not its re-signed SHA.** The per-cycle re-sign
  rewrites commit SHAs, so an agent that wrote its commit's SHA into log.md got its task spuriously
  reopened by signoff ("that commit doesn't exist"). The work prompt now points agents at the stable
  `Coop-Task:` trailer, and signoff finds the commit by that trailer instead of a volatile SHA.

- **`coop acp` picks up a rebuilt coop on SIGHUP without dropping the editor.** Zed owns the
  `coop acp` process — its stdio IS the transport — so the old way to load a new binary,
  `pkill -f 'coop acp'`, killed that process and Zed (which never relaunches an agent server that
  exits) failed every later request until you restarted the editor. Now `pkill -HUP -f 'coop acp'`
  re-execs the freshly-installed binary IN PLACE (`syscall.Exec` — same PID, same fd 0/1/2, so the
  editor's pipe never breaks), tears down the old box, and re-establishes your already-open threads
  against a fresh box on the new binary by reusing the box-restart replay (initialize + session/load
  + config_option_update). SIGTERM/SIGINT still STOP it — reload is a distinct signal, so coop stays
  stoppable. The handoff rides a mode-0600 temp file that's deleted after one read; a corrupt/absent
  one degrades to a fresh start rather than crashing.

- **ACP provider switches are near-instant — coop keeps a box warm per signed-in provider.** A
  switch was ~5s: a warm container is ~0.9s, but the rest was the node adapter cold-start +
  initialize/session-load replay. coop now pre-spawns a box for each OTHER signed-in provider in the
  background at session start (the active one starts as fast as before), so a switch swaps to a
  hot adapter and pays only the replay. It lives behind the existing supervisor factory — a miss
  cold-spawns exactly as before, so correctness is unaffected — and boxes are reaped on session end.
  `COOP_ACP_WARM=0` opts out (one fewer idle box per provider on a low-RAM machine).

- **`coop init` scaffolds per-agent dirs only for the agents you use.** It now creates
  `.claude/`/`.codex/`/`.gemini/` only for the agents you're signed in to — or the `--agents
  claude,codex` (or `all`) you name — instead of all of them. `.agent/` is always scaffolded; the
  un-scaffolded agents aren't clutter you carry, because a box synthesizes a missing agent's skills
  from `.agent/skills` on demand. A repo that never uses an agent can delete its dir (the one thing
  that needs it back is running that agent's own CLI on the host). With no agents signed in and no
  `--agents`, it scaffolds `.agent/` only.

- **`.agent/` is now enough — a box synthesizes an agent's skills from `.agent/skills` when the repo
  has no per-agent dir.** A repo that keeps only vendor-neutral `.agent/` (no committed
  `.claude/`/`.codex/`/`.gemini/`) still gives the agent its workflow skills: coop mounts
  `.agent/skills` at the box's user-level `~/.<agent>/skills`. When the repo HAS the agent's own
  skills dir, that project copy wins (nothing synthesized). It's a writable ephemeral copy, not a
  read-only bind — a CLI that installs its own system skills into that dir (codex) isn't broken, and
  the host's `.agent/skills` stays pristine. Verified end-to-end in a real box. (Skills first; the
  settings/hooks equivalents and the `coop init` scaffolding change are filed follow-ups.)

- **The loop now flags when a task edits its own verifier.** In an unattended loop a candidate can
  weaken the checker to pass itself — edit `*_test.go`, the `Makefile`/gate, `.agent/project.yaml`,
  `.agent/loop.yaml`, `.claude/hooks/`, or CI — and cross-vendor review is no defense when every
  reviewer trusts the same mutable oracle. coop now DETECTS (host-side, deterministically) when an
  iteration's commits touched a gate-defining file, warns, and tells the review to verify the gate
  wasn't weakened rather than the code fixed (a per-task note in the between audit, plus a standing
  gate-integrity directive in every review). This is the boring first step — the stronger dual-run
  / approval enforcement is a filed follow-up pending the enforcement-model decision.

- **The box image is ~470MB smaller (3.16GB → 2.69GB) on a `node:24-slim` base.** The full `node:24`
  base shipped build-essential/git/python/mercurial/openssh that the box's own apt layer already
  re-installs — so those installs were near-no-ops on a fat base. On slim the box installs exactly
  what it uses, and the base's unused cruft is gone. mercurial and openssh-client drop for free
  (they came only from the full base; openssh is unusable in the box anyway — coop shadows `~/.ssh`
  and holds no key). Verified: every agent tool still present (git, curl, python, gcc/make, ripgrep,
  fd, jq, tree, psql, asdf, the four agent CLIs) and git-over-HTTPS works; glibc kept (Debian slim,
  not alpine) so asdf toolchains and prebuilt binaries are unaffected.

- **`coop sign` re-signs your unpushed commits with your host key — and the loop does it per cycle.**
  Box commits are made unsigned (no key ever enters a box), so a remote that requires signed commits
  (a protected `main`, like many projects) rejects them. `coop sign` re-signs the unpushed range —
  `@{upstream}..HEAD`, or `--from <ref>` when there's no upstream — on the host, using your GLOBAL git
  signing config (so a poisoned repo can't plant a `gpg.program`). It never pushes, never rewrites
  pushed history, and refuses a dirty tree or a range with a merge commit. `coop loop` signs each
  cycle's commits the same way when you sign by default (`commit.gpgsign=true`), with an end-of-run
  sweep for stragglers — best-effort, so a signing hiccup warns rather than derailing the run. The
  `Coop-Task` trailer survives the re-sign, so the commit↔task binding is unaffected.

- **Loop commits are now bound to their task, and the queue self-repairs.** Every loop commit ends
  with a `Coop-Task: <id>` trailer (the work prompt and AGENTS.md instruct it), so the host can
  finally map a commit to the task it completed — previously nothing did (`git log --grep <id>` was
  empty repo-wide). Three repairs ride on it: **informed resume** — an in_progress task that already
  has a landed commit (a crash after commit, before the folder-move) tells the next agent to
  verify-and-finish or, if the review reopened it, rework — instead of blindly redoing it;
  **post-fork-merge reconciliation** — when a fork lands, a parent-queue task whose trailer is now in
  history moves to done automatically (a blocked one is flagged, never moved), so the parent loop
  doesn't redo landed work; and **attempt evidence** — each work telemetry row records the tasks the
  iteration finished and any it finished with no Coop-Task commit (unbindable), which also warns
  loudly. The LLM still moves the folders; the controller supplies the evidence and repairs the drift.

- **`coop loop` now records one telemetry row per stage.** Each preflight, work, between-audit, and
  signoff stage appends a JSON-Lines record to `.agent/runs/<run>.jsonl` (gitignored): the
  **effective** target that actually ran (provider/model/effort/account, post rate-limit rotation —
  not the configured ladder), coop version, start/end, exit, retries, HEAD before/after, the queue
  counts, and — for a signoff — how many tasks it reopened. It's the raw material for measuring the
  harness itself (audit catch-rate, reopen rate per model, retry cost). Best-effort: a telemetry
  write failure warns once and never touches the run. (Phase 1 — a replay/canary set over the
  archive is a separate follow-on.)

- **Box commits now carry one coop co-author trailer, replacing the agent CLI's own.** Every commit
  made inside a box gets `Co-authored-by: coop (<provider>:<model>@<account>) <noreply@coop.dev>`
  attributing coop and the exact target that ran — via a `prepare-commit-msg` hook mounted into the
  box (so it works for every agent, and even under `git commit --no-verify`). The agent CLIs' own
  machine co-author lines (Claude, Codex/ChatGPT, Gemini, Grok) are stripped; a human `Co-authored-by`
  line survives, `--amend` stays idempotent, and merge/squash messages are left untouched. The host
  repo's git config is not touched, so host-side operations (including a fork-merge rebase) are
  unaffected.

- **`coop fork merge` can no longer erase a commit that lands on the parent while the gate runs.**
  The merge used to fast-forward the fork into the *live* parent, then run the gate there, then
  `git reset --hard` on failure — so a commit you (or another fork) made to the parent during that
  minutes-long gate was discarded by the rollback. Now the rebase and the gate happen entirely in
  the fork's own clone (the candidate), the parent is advanced only by a fast-forward-**only** merge
  once the gate is green, and that ff-only IS the compare-and-swap: if the parent moved, it refuses
  and nothing lands (re-run to rebase onto the new HEAD) rather than rolling a shared tree back over
  the concurrent work. A red gate now leaves the parent completely untouched — there is nothing to
  reset. The gate policy is still read from the parent, so a fork can't weaken its own checker.

- **A signoff that decides reopens but moves no folders is no longer silently accepted.** After the
  2026-07-10 incident — an end-of-loop review collected six REOPEN verdicts as prose, its
  background subagents were killed by an output-limit restart before the folders moved, and the
  loop saw an empty queue and accepted the batch — the review now must end with a
  `REVIEW COMPLETE — reopened <N>` receipt. The loop compares N against the task folders that
  actually moved back to `10_in_progress/`; a mismatch or a missing receipt means a verdict may
  have been lost, so the round is re-run within the cap, or (at the cap) the loop exits loudly for
  a human instead of claiming a false "done". A PASS that merely mentions reopening in prose still
  passes — only the receipt's count is load-bearing.

- **The loop's between/signoff reviews now actually run on their configured target — and fail
  closed.** `stepModel` kept only `(model, effort)` off a review stage's `agent:` ladder and pasted
  it onto the *work* provider, discarding the provider, account, and fallback rungs — so a
  claude-led run's `codex:gpt-5.6-sol` signoff resolved to `claude --model gpt-5.6-sol` (an invalid
  pairing), and the cross-vendor reviewer the config promised was never actually launched. Reviews
  now build their own rotation from the ladder — the real provider, model, effort, account, and
  fallback rungs, rotating them on a rate limit like the work loop. And a review that can't run (a
  launch error or a nonzero, non-limit exit) is retried, then stops the loop loudly for signoff or
  warns "left unaudited" for a between audit — never mistaken for "nothing reopened, accepted".

- **`coop credentials` now shows how stale each token is.** Every signed-in credential renders a
  `rotated <age>` field — the mtime of its token material (claude's `.credentials.json`, codex/grok's
  `auth.json`), which any login or OAuth refresh bumps. It answers "how long could a leaked token
  still be used" at a glance, so rotating one to contain a blast radius is an informed call; an
  env-key login with no marker file shows `—`.

- **A preset now owns the whole ACP toolbar.** With a preset active, its lead ladder owns the
  provider, model, effort, account, and roles — so the toolbar renders only the Preset dropdown
  (the native model/effort were already hidden). Switching back to None restarts the box and its
  config-update brings Provider, Account, and the native model/effort back.

- **Parallel codex boxes on one account no longer crash each other.** codex ≥0.144 keeps
  single-writer sqlite state (`state_*.sqlite`, logs, memories, goals) in `$CODEX_HOME`, so a
  second box mounting the same account's home died at startup with a cryptic `failed to initialize
  sqlite state runtime` (a second Zed thread, an ACP provider switch, a loop beside a session) —
  sqlite's writer lock can't span the shared bind mount. coop now points that state at a
  container-local path via codex's own `CODEX_SQLITE_HOME` (honored by codex and codex-acp), so
  each box keeps its sqlite on its own writable layer and the shared home — auth and its in-place
  refresh, `sessions/`, config — stays mounted exactly as before. Run as many codex sessions on
  one account as you like; login and resume (rebuilt from the shared session rollouts) keep
  working. Per-box goals/memories don't persist, which is inherent to parallel sessions.

- **An ACP session that dies in a box restart now says so in the thread.** When the respawned box
  couldn't re-establish a session (e.g. a box that fails to start), the failure went only to stderr:
  the toolbar silently kept coop-only dropdowns (no model/effort) and every later prompt or
  provider switch failed against a dead session. The proxy now posts the actual error into the
  thread with what to do next.

- **A provider switch no longer flickers the old provider back in the toolbar.** The ack to a
  `coop_provider` switch was built from spawn-time state, so the dropdown echoed the previous
  provider until the respawn's config update landed. The switch now retargets coop's per-lead
  state before acking — the ack renders the new provider and its accounts immediately.

- **A provider switch no longer keeps the OLD provider's model menu.** The refreshed toolbar
  echoed the cached native options — switch claude → grok and the model dropdown still listed
  Opus/Fable/Sonnet. The stale menu is now dropped the moment the provider switches; the new
  box's own options (grok: `grok-4.5` / `grok-composer-2.5-fast`) replace it when its truth
  arrives.

- **The Preset dropdown now leads the normal ACP toolbar.** A preset is the top-level selector — it
  embeds the provider, model, effort, account, and roles — while an active preset renders only
  Preset, matching what it owns.

- **A credential/preset switch mid-turn no longer kills the turn.** Switching the coop toolbar
  dropdowns while a reply was streaming failed the in-flight prompt with `-32000 agent restarted,
  please retry` — the thread looked crashed and the answer was lost. The switch now arms the same
  transparent resend a rate-limit rotation uses: the prompt is re-sent once the new box is up
  (with the carried-history preamble when the session was re-created), and its answer completes
  the editor's original request.

- **No more doomed reloads after a session re-create.** A re-created session (a provider switch)
  kept its "has a transcript" flag, so every following switch first tried a `session/load` of a
  box id that was never persisted, failed, and warned "did NOT reload — re-creating" — even
  same-provider. The flag now resets on re-create: the next switch re-creates in round one,
  no false warning, one less replay round-trip.

- **A selected preset hides the native model/effort dropdowns.** The preset's ladder and roles own
  the model — the box-native knobs were inert at best and misleading at worst, and an editor
  replaying its persisted defaults (Zed does, on every new session) could silently override the
  preset's pick in the box. Under a preset only coop's Preset selector renders; native and hidden
  Provider/Account sets from the editor are answered by coop instead of reaching the adapter or
  restarting. Leaving the preset brings the normal and native dropdowns back.

- **The repo's `agents/` examples folder is retired.** Its README documented an `install.sh`
  seeding into `~/.config/coop/agents/` that no longer exists (installs download a prebuilt
  binary), and each file had become redundant: the box briefing already carries the example's
  sandbox note and the scaffolded `AGENTS.md` its agent-stack habits, `coop init` seeds the
  `mcp.json` stub, and the README shows the env-file one-liners. What was unique moved into the
  README's "Agents & config" section: the per-agent instruction override (a `CLAUDE.md` in
  `agents/claude/` beats the shared `INSTRUCTIONS.md`), a sample `mcp.json`, and the MCP
  `env`/`bearer_token_env_var` mechanics. Runtime behavior is unchanged —
  `~/.config/coop/agents/INSTRUCTIONS.md` and the `env` file work exactly as before.

- **One `.agent/loop.yaml` replaces the `.agent/loop/*.md` files — with a per-step model ladder and
  prompt for each of preflight / work / between / signoff.** The three markdown knobs
  (`review.md`/`audit.md`/`between.md`) collapse into one committed YAML with a section per step:
  each takes an `agent:` ladder (`provider[:model][/effort][@account]` targets **or** a preset name
  — a preset rung runs that step under the preset's roles + lead ladder, rotated on a rate limit),
  and a prompt. The end-of-loop pass is named **`signoff:`** — between is the per-task reviewer,
  signoff the final one, so neither reads as "the" review. Prompts never OVERRIDE a coop built-in —
  `signoff.prompt` and `preflight.prompt` **append** to theirs; `between.prompt` **sets** the
  per-task audit (between has no built-in; coop prepends the just-finished task's id + folder —
  the audit's exact subject, so the prompt never has to guess "the most recent" task).
  Settings live here too: `signoff.rounds` (was `COOP_MAX_REVIEW_ROUNDS`), `preflight.enabled` (was
  `--preflight`/`COOP_PREFLIGHT`, still overridable), `work.command` (was `COOP_LOOP_CMD`). A missing
  file or field = today's built-in defaults, so an absent `loop.yaml` changes nothing. The retired
  `.agent/loop/*.md` (and legacy `.agent/audit.md`) tombstone once if left behind. `coop init` now
  scaffolds a fully-commented `.agent/loop.yaml`, a committed `.agent/project.yaml`, and an empty
  `.agent/presets/`. The five loop env vars are RETIRED (gone, not read) — `COOP_LOOP_MODEL` →
  `work.agent`, `COOP_REVIEW_MODEL` → `signoff.agent`, `COOP_MAX_REVIEW_ROUNDS` → `signoff.rounds`,
  `COOP_LOOP_CMD` → `work.command`, `COOP_PREFLIGHT` → `preflight.enabled`.

- **A repo can commit its box policy + merge gate in `.agent/project.yaml`.** Alongside
  `subprojects`/`serve`, project.yaml gains a `box:` section (`egress`, `auto_up`, `network`,
  `memory`, `cpus`, `pids`) and a `gate:` — so a team pins "this repo's agents run offline" or
  "merges revalidate with `make check`" once, committed, instead of each dev setting env vars.
  Precedence is coop's usual **explicit `COOP_*` env/conf › this file › built-in default**, applied
  on a copy of the config inside `box.Run` (the shared config is never mutated). Because the
  committed file is read on the host from a possibly-untrusted repo, it can only ever TIGHTEN the
  posture: `egress`'s default is already the loosest (`open`), so a repo may pin `none` but never
  widen your explicit `none`, and `no_new_privileges` is deliberately not a key. The `gate:` runs
  in the box (sandboxed) and an explicit `COOP_GATE` still wins. `project.Load` now rejects unknown
  keys, and `coop init` scaffolds a commented `box:`/`gate:` block.

- **A bare `coop acp` (no provider) now starts on your first signed-in provider instead of
  erroring.** v4.0.0 made the provider required everywhere, so an editor `agent_servers` entry of
  just `["acp"]` failed fast with a usage error. The ACP surface is special — it has a live PROVIDER
  dropdown — so a bare `coop acp` now defaults to the first signed-in agent and the dropdown
  switches it from there (a wrong guess is one click to fix, unlike `coop claude`/`coop loop`, which
  stay strict since they have no such correction). Nothing signed in still errors, now pointing at
  `coop login`. Naming a provider (`coop acp claude`) or a `--preset` still pins it.

- **Sibling `compose.agent.yml` files are now VALIDATED on the host instead of shadowed in the
  box — no more phantom compose files, and agents can scaffold services.** coop auto-runs a repo's
  compose file on the host daemon, which is host-root-equivalent (`privileged`, `/:/host`,
  `/var/run/docker.sock`, `network_mode: host`, `env_file: ~/.ssh/…`), so v4.0.0 shadowed the two
  compose paths read-only in the box to stop an in-box agent authoring one. But Docker materialized
  each read-only mount target inside the repo, stranding an empty `compose.agent.yml` (and
  `.agent/`) on the host at every launch — debris a long-lived ACP session pinned for its whole
  life. The shadow is gone. Instead, `box.EnsureServices` validates the file before every
  `compose up` (auto-up and `coop up`): it parses against a strict allowlist of safe
  sibling-service directives (`KnownFields(true)` deny-unknown, so `privileged`/`build`/`env_file`/
  `driver_opts`/`network_mode`/… are rejected for not existing in the schema — including compose
  features not invented yet), plus bind sources must resolve within the repo (symlinks and
  `${interp}` included) and published ports must bind loopback only. A refused file names the exact
  offending key/path; `coop up` prints it, auto-up warns and starts the box without services. Net:
  the compose path is writable from the box (an agent can add a Postgres sidecar), the file is safe
  to auto-run whoever wrote it, and no empty compose files ever appear in the repo.

- **The sibling-services compose file is now ONLY `.agent/compose.yml`, and it's committed.** coop
  read two paths (root `compose.agent.yml` and `.agent/compose.yml`); the root one is retired, so
  there's a single home for it, tucked in `.agent/` beside `loop.yaml`/`project.yaml` rather than
  cluttering the repo root. `coop init`'s `.gitignore` block un-ignores it (`!**/.agent/compose.yml`)
  so a repo's services config is tracked like its other committed knowledge; a pre-existing block is
  upgraded in place. A root `compose.agent.yml` is no longer picked up — move it to `.agent/`.

- **ACP sessions can switch PROVIDER — manually or on a rate limit — with the thread carried
  best-effort.** A normal editor toolbar has THREE coop dropdowns mirroring the target grammar —
  **Provider** (who runs), **Account** (the lead's login), **Preset** (the recipe) — one selection
  underneath, so changing one refreshes the others (the old mixed `coop_setup` dropdown is gone;
  persisted editor defaults are still accepted on set). An active preset is the exception: only
  `coop_preset` renders, and persisted Provider/Account sets are acknowledged as no-ops. A preset's
  cross-provider ladder rungs rotate on ACP like they do on the loop. A provider switch
  re-creates the session on the new agent (the old provider's transcript is unreadable to it —
  see the 2026-07-04 investigation) and carries the conversation as a labeled plain-text
  preamble on the first prompt: coop retains a per-session history of (user, assistant) texts
  plus one-line tool NARRATION (`[tool] Read main.go — completed`; payloads excluded — they
  dominate transcripts and go stale) from the wire it already proxies, budgeted by
  **`COOP_ACP_CARRY_TOKENS`** (default 200000 ≈ 800KiB; the preamble says when earlier context
  was evicted — trim the budget below the smallest window you switch into). User asks keep
  their head, assistant turns their tail, "approximate" said out loud. Same-provider switches
  stay fully transparent (the shared session store, as before). Under the hood: one respawn
  env `COOP_ACP_TARGET` in the target grammar replaces
  `COOP_ACP_CREDENTIAL`/`COOP_ACP_LEAD_MODEL`/`_CRED`; the control re-derives its per-lead
  state (accounts, models dropdown, limit signals) at every spawn; and the proxy now
  RE-CREATES a session whose `session/load` fails after a restart (new id, remapped, loss
  named on stderr) instead of leaving it dead — that fallback fires for any lost transcript,
  not just provider switches. `coop fusion` still refuses a cross-provider governor ladder.

- **One `Target` type end to end.** The three parallel spellings of "who runs on what" —
  `preset.ModelTarget`, the loop's internal `runTarget`, and the raw strings between them — are
  gone; presets, the loop's rotation, ACP failover, and fleet entries all carry the ONE
  `agents.Target` that `ParseTarget` produces (`Target.Account()` is the single-account rung
  view). A preset's `LeadLadder` keeps whole targets (accounts fan out at run time against
  what's signed in), `coop presets <name>` prints the ladder in full target form
  (`ladder claude:fable-5, claude:opus@work`), and a malformed target now reads IDENTICALLY on
  every surface — CLI positional, preset `lead.agent`, role `agent:`, fleet entry — including
  `coop loop gpt4`, which now says `unknown provider "gpt4" — use claude, codex, gemini, grok`
  instead of a generic "unexpected argument" (one tested cross-surface error suite pins this).

- **Cross-provider lead ladders are bounded to the surfaces that can honor them.** The loop
  embraces them (rotation swaps the agent per rung). `coop fusion` now REFUSES a preset whose
  lead ladder spans providers — fusion runs one governor for the whole council, so a foreign
  rung could never apply and silently ignoring it would fake a fallback. An ACP session honors
  the preset's full cross-provider lead ladder (its respawn env carries the active target), so
  failover keeps working across the lead's models/accounts/providers. A preset ROLE's `agent:` list now gets a purposeful
  rejection ("a role runs ONE target; fallback ladders belong to the lead") instead of a raw
  YAML type error — nothing rotates a role today, and dead config that looks like failover is
  worse than an honest no.

- **Sign-in guidance speaks the target grammar.** The `coop credentials` re-login/default hints
  still recommended the retired `--credential` flag (`coop login claude --credential work`);
  every one now says `coop login claude@work`.

- **The loop review must execute each reopen the moment it decides it — batched verdicts were
  silently lost.** A real review run fanned its per-area reviews out to background subagents,
  collected REOPEN verdicts as prose, and deferred every folder move until "after the final
  child reports" — then an output-limit resume (each resume restarts the agent process, killing
  its background children) left it waiting on children that no longer existed; it eventually
  stopped, the loop saw zero reopened folders, and accepted the batch — six recorded REOPENs
  evaporated. The default review prompt and the fixed context footer (which also binds under a
  custom `.agent/loop/review.md` override) now demand the move + log note IMMEDIATELY per
  verdict, never batched for the end, and forbid parking verdicts behind background subagents —
  work still running when the turn ends dies with it.

- **GitHub release pages show the changelog again, not an empty body.** v4.0.0 published with an
  empty release body (only the compare-link footer): `.goreleaser.yaml` set `changelog.disable:
  "true"`, and GoReleaser's changelog pipe — the same one that reads the workflow's
  `--release-notes` file — is skipped wholesale when disabled, so the extracted `CHANGELOG.md`
  section was dropped. The disable is gone; a provided notes file is used verbatim and still
  returns before commit/PR generation, so a direct-commit repo can't leak dependabot-bump noise.

## 4.0.0

- **xAI's Grok Build (`grok`) is a fully baked-in provider.** `coop grok` (and fusion/fork/loop/acp,
  `--consult grok`, presets) run it like any other agent. grok ships a piped installer
  (`curl … | bash`), not npm and not a checksummed release, so — matching how grok distributes —
  coop bakes that into the box image rather than inventing a checksum grok doesn't publish; the
  installer's `/usr/local/bin/grok` symlink into root's home is replaced with a world-executable
  copy so the box's `node` user runs it. Verified end-to-end on a host box build: the image builds,
  `grok` runs as `node`, and `coop credentials` detects its `auth.json`. Sign in with `coop login grok`.

- **One way to name a run: the target grammar `provider[:model][@account]`, and peers that
  participate only when named.** Every launch surface — `coop <agent>`, `loop`, `acp`, `fusion`,
  `fork <name> [acp]`, `login` — now takes a single **target** for WHO runs: `claude`,
  `claude:opus`, `claude@work`, `claude:opus@work`. **The provider is required** (no more
  implicit `claude` default — a bare `coop`/`loop`/`acp`/`fusion` names the fix), while the model
  stays optional (it falls to the agent CLI's default). **`--model` and `--credential` retire**
  everywhere — they were just target segments (`claude:opus@work`); each tombstones with the
  rewrite. `coop login claude@work` replaces `coop login claude --credential work`. An account
  **ladder** rides the target too (`claude@work,personal`, rotated by `coop loop` on a rate limit).
  Peers are now **explicit**: repeatable `--consult <peer>` on `coop <agent>`/`loop`/`acp`/`fork
  --loop` and repeatable `--peer <peer>` on `coop fusion` (≥1 required — a bare fusion errors); the
  old boolean `--consult` and the implicit "every signed-in agent is a peer" policy are gone. The
  **security dividend**: a run mounts the credentials of exactly its lead + named peers + a preset's
  role agents — an agent the run never named never enters the box, and the in-box `coop-consult`
  refuses (via `COOP_PEERS`) any target not in this run's council, so a compromised or confused
  lead can't consult (and thereby drive) an unlisted agent. `--preset` is unchanged (a preset is an
  orthogonal axis — role wiring — not another spelling of the target); a per-fork `consult: true` in
  `.agent/fleet.yaml` now refuses at `coop fleet up`. See MIGRATING.md.

- **Presets speak the same target grammar — `agent:` holds a target, and `model:`/`models:` retire.**
  A role's `agent:` is now a target (`agent: codex:gpt-5.5`) — the model rides the same key as
  everywhere else instead of a separate `model:` (a role runs its agent's default account, so no
  `@account`). The lead's `agent:` holds a target or a same-provider fallback **ladder**
  (`agent: [claude:fable-5, claude:opus-4-8@work]`), folding in the old `models:` list. Both retired
  keys tombstone with the rewrite. A lead ladder may even be **cross-provider**
  (`agent: [claude:opus, codex:gpt-5.5]`) — the loop rotates across vendors on a rate limit, running
  each rung's own agent (the lead, and a single run, use the first rung). **`.agent/fleet.yaml` follows suit**: a fork's
  `agent:` is a target (`agent: gemini:gemini-3.5-flash@work`) and the per-fork `model:`/`credential:`
  keys retire — the model + account ride `agent:` (a fork still takes one account; a rotation lives in
  a preset).

- **`coop loop`'s end-of-loop pass is now a customizable, DEMANDING review that loops until it
  accepts.** Commit `.agent/loop/review.md` to FULLY override the review prompt (committed config,
  like a preset — the scaffold `.gitignore` allowlists `.agent/loop/`), while `.agent/loop/audit.md`
  appends as the light "add a check" layer, and coop always appends a fixed context footer (the queue
  paths, `AGENTS.md`, and the "coop isn't installed — reopen by moving the folder" mechanics). The
  **default review is now a senior reviewer's bar**: per `99_done/` task it verifies the goal is met
  (every acceptance criterion + subtask), the standards are followed (`AGENTS.md` + `.agent/rules`, no
  scope creep), the **failure path** is tested, and the change is polished (docs/CHANGELOG updated),
  plus bookkeeping — then runs the repo's gate **once** across the whole repo (not per task),
  reopening anything short of "merge with no changes"; it still never fixes task code itself.
  Structural **loop-until-accepted**: after the review the loop re-drains anything reopened and
  reviews again until a review reopens nothing. The round cap now **scales with the batch** —
  `clamp(tasks worked / 2, 3, COOP_MAX_REVIEW_ROUNDS)`, and `COOP_MAX_REVIEW_ROUNDS`'s default rose
  **3 → 5** — so a small batch still gets a few tries and a 100-task overnight run can't ping-pong one
  stuck task forever; on cap-exceed the persistently reopened task is blocked for a human
  (`50_blocked/` + a `decision.md`) so the loop exits 3, not a false "done". New knobs:
  **`COOP_REVIEW_MODEL`** runs the review pass (and the between audit below) on its own, typically
  stronger model — a capable model reviews the cheaper loop's work; and an opt-in
  **`.agent/loop/between.md`** runs a per-task audit AFTER each completed task (its text is the
  prompt) that reviews the just-finished task and may reopen it before the loop moves on (absent = no
  between-task step). The review's project checks **moved `.agent/audit.md` → `.agent/loop/audit.md`**
  (beside `review.md`/`between.md`); the old path is no longer read and coop warns once if it lingers.

- **`coop models` shows LIVE models, not just a hardcoded list — one readable block per agent.**
  Each agent gets a block: a bold-cyan header, its `Models:` (the real cached catalog when a fresh
  one exists, else the curated static examples), and an explicit `Last refreshed:` fact — a green
  age when live, `never`, or a yellow stale age, with the refresh channel as the dim hint — coop
  still never validates `--model`, so any id the CLI accepts works either way. The list is fetched
  auth-free: claude/gemini populate the cache for FREE on every `coop acp` session (the proxy
  already parses the ACP `session/new` models), and the new `coop models --refresh` runs
  grok/codex's native CLI on the host (`grok models`, `codex debug models`), folding each outcome
  into that block's `Last refreshed:` line. Plain `coop models` stays instant and never touches
  the container runtime — it only reads a per-agent cache under
  `~/.config/coop/agents/<agent>/models_cache.json` (14-day TTL). Every refresh path is
  best-effort: an absent CLI, timeout, or parse error falls back to the cache-then-static list,
  noted on the block, and never errors or hangs. (A `--refresh` that drives claude/gemini through
  a box ACP handshake is deferred — their cache already refreshes on `coop acp`.)

- **Presets can live globally in `~/.config/coop/presets` — shared across every repo.** A preset
  is looked up in the repo's `.agent/presets/` first, then the global root; on a name collision the
  repo wins (so a project can override a personal recipe). `coop presets` marks global-sourced
  recipes `(global)` and `coop presets <name>` prints the resolved path; a global preset's
  `roles/*.md` prompt files resolve under its own folder. Relocate the root with
  `COOP_PRESETS_DIR`. A repo with no global dir behaves exactly as before.

- **ACP: gemini gets a real model dropdown in the editor toolbar.** gemini's adapter reports its
  models via ACP's `models` field (not a `configOptions` entry), so Zed showed no model picker for
  it. coop now synthesizes the dropdown from that field and translates a pick into the adapter's
  `session/set_model`, re-applying the choice after a box swap — claude's and codex's native model
  options are untouched.

- **The ACP path's host-side fs/terminal boundary is now documented.** Over ACP the editor
  services `fs/read_text_file` / `fs/write_text_file` / `terminal/*` requests host-side, and
  coop's proxy forwards them to the editor unfiltered — so when you drive an agent from Zed, the
  box's isolation is only as strong as the editor's own fs/terminal sandbox (a prompt-injected
  agent could ask the editor to touch an absolute host path outside the repo). The README's ACP
  section now spells this out; `coop loop`/`coop claude` have no such channel and are unaffected.

- **Security: an in-box agent can no longer plant a compose file for coop to auto-run on the
  host.** coop's auto-up runs `compose.agent.yml` / `.agent/compose.yml` on the HOST daemon before
  a networked box starts, and those paths live in the read-write repo mount — so a prompt-injected
  agent could author one (`privileged: true`, a `/` bind mount) and have the next launch execute it
  host-side. The box now shadows both paths read-only: an existing file stays usable exactly as
  committed, but an agent can't create or modify one from inside the box.

- **A rate-limit wait no longer over-waits across a laptop suspend.** The loop's `sleepForLimit`
  and the ACP respawn's `sleepUntilReset` built their countdown on Go's monotonic clock, which
  *freezes* while a macOS lid is closed — so a "waiting Nh until `<reset>`" pause counted only
  *awake* time and kept waiting past the real reset by roughly the closed duration. Both now anchor
  to a WALL-clock deadline (monotonic reading stripped) and re-check it on short (≤1m) ticks, so
  reopening the laptop past the reset ends the wait within a tick; the unknown-reset path still
  backs off by duration, and Ctrl-C / ctx-cancel still bail promptly.

- **Unattended runs can no longer spin forever on a wedged limit.** Two give-up caps: the loop's
  output/token-limit path (which retried the same iteration immediately, forever, if a model kept
  maxing out or a failing gate echoed `finish_reason: length`) now backs off after the first
  consecutive hit and stops after 5, and the transparent ACP failover (which could
  respawn→wait→resend indefinitely when every credential stayed limited with no reset time) forwards
  the real limit error to the editor after 12 consecutive all-limited waits. A completed turn or a
  free rotation resets either chain. The ACP path also no longer swallows an "approaching your
  limit" advisory when a turn ends in a non-limit error — it's flushed ahead of the failure.

- **`:d` in `coop tasks decisions -i` now DELETES the current decision's task, not marks it done.**
  `:d` read as delete/drop, so v3's "mark done" mnemonic was backwards; the mark-done shortcut is
  dropped entirely (no replacement key). A decision is now closed by *answering* it (records the
  resolution, unblocks to todo) or by `:d` deleting it. Delete is unrecoverable, so it confirms
  inline first (`delete <id>? [y/N]`, default No — a stray Enter cancels), reading the y/N from the
  browser's own input so a declined confirm is a safe no-op that stays on the decision. The closing
  summary counts answered/deleted (the done count is gone).

- **`coop fork --loop` is monorepo-aware by default.** With no `--tasks`, a fork now seeds
  *every* queue coop knows about — the repo's own `.agent/tasks` plus each `.agent/project.yaml`
  subproject's queue, each at its own relative path — so a monorepo fork carries all its
  subprojects' work and the in-fork loop aggregates them exactly like `coop loop` does (a task
  never leaves its home queue, so colliding ids across subprojects can't mix). An explicit
  `--tasks <path>` still hands the fork one queue. A single repo is unchanged (`.agent/tasks`).

- **`coop fleet split` is removed.** It dumb-round-robined one queue into per-fork slices and
  wrote `.agent/fleet.yaml` for you; a fleet is now always authored explicitly (`coop fleet init`
  writes the template). `coop tasks split <n>` still slices a queue into copy-trees for a
  hand-wired fleet.

- **A `/sweep`'s Stop-guard no longer holds other sessions hostage.** The `.agent/active`
  sentinel was repo-global: a sweep armed it and every concurrent session in the repo (say, Zed on
  the host) got its Stop hook blocked too, and a crashed sweep left the flag stuck until deleted by
  hand. The sweep now writes its own session id into the marker and the guard releases any session
  that is provably a different one — the sweep's own hold is unchanged, and re-arming self-heals a
  stale marker.

- **`coop tasks add` no longer drops repeated section flags.** Passing `--acceptance` (or
  `--context`/`--approach`) more than once silently kept only the LAST value; repeats now accumulate
  as paragraphs under the one heading, like `--subtask` always did.

- **Future GitHub releases get real notes.** The release workflow now renders the release body from
  `CHANGELOG.md`'s top section at the tag (and fails loudly if it's empty) instead of GitHub's
  PR-label notes — which, for a repo of direct commits, published v3.0.0 with two dependabot bumps
  as its whole story.

- CLI polish: `coop tasks queues` completes and typo-suggests; `coop loop <TAB>` no longer offers
  the retired `pool`; `completion` is listed once; and `coop tasks decisions -i` draws a bold
  colored divider between decisions so the boundary is visible while you answer.

- `coop fork review` prints a review dossier, not just a brief + diff. Between the commits and
  the patch it now maps the risk, all parent-computed from git facts: the fork's task log is
  labeled as the agent's claim (it's the fork's own voice — it can't steer its review); policy
  findings from the exact scan `coop fork merge` enforces print as advisory ⚠ lines at review
  time instead of first surfacing as a failed merge (a clean fork gets `✓ nothing flagged`);
  the changed files are risk-ordered — config & instructions (AGENTS.md/`.agent/`/`.claude/`…,
  dependency manifests + lockfiles, Dockerfile*, Makefile, CI) first, then code by churn, then
  tests, then docs, each with `+N -N`; and a `gate:` line says whether a merge gate is
  configured. No new flags; `--stat`/`--tool`/`--open`/`COOP_REVIEW_CMD` behave as before.

- `coop tasks lint` now flags a task id that sits in more than one state dir (a `cp` instead of a
  coop move). The queue readers deliberately show such a task once — that dedup protects the loop,
  `coop tasks watch`, and the Stop hook from torn mid-rename reads — so a persistent duplicate was
  invisible everywhere; lint re-checks each candidate (a task mid-move exists in at most one dir at
  any instant) and reports only real copies, naming the dirs and which copy the listing shows.

## 3.0.0

coop v3 is a clean break organized around three ideas: the work queue is folders, not a
file; a stored account (a credential) and an orchestration recipe (a preset) are two
different things; and model is the one rotation axis. The CLI ships no backward-compat
aliases — every retired spelling exits with the exact rewrite to run instead — and
[`MIGRATING.md`](MIGRATING.md) walks each conversion.

### Breaking changes

- **Tasks are folders now (`.agent/tasks/`); the single `.agent/TASKS.md` is gone.** One
  folder per task under four state directories — `00_todo/`, `10_in_progress/`, `50_blocked/`,
  `99_done/` — and a task's state IS which directory it sits in: every transition is an atomic
  folder move, there is no `status:` field, and there is no legacy fallback. Each task carries
  its own `spec.md`, `log.md`, resume `state.md`, `decision.md` (replacing the global
  `PENDING_DECISIONS.md`), and `screenshots/`/`artifacts/`. `coop tasks` drives it all
  (`add`/`claim`/`block`/`unblock`/`done`/`rm`/`ls`/`lint`/`decisions`/`path`), and the loop,
  fleet, Stop hook, and `coop init` are folder-native. A finished task is moved to `99_done/`,
  never deleted; prune by hand with `coop tasks rm --all-done`. MIGRATING.md has a one-shot,
  LLM-ready conversion prompt for a legacy `TASKS.md`.
- **`coop profiles` is now `coop credentials`; recipes are presets.** A credential is a stored
  account/login (its own rate-limit pool); a preset is the orchestration recipe. `coop profiles`
  tombstones with the rewrite; the `--profile` flag is removed outright (`--credential` is the
  one name — an agent's own `--profile` still passes through after `--`). New `coop presets
  [name]` lists/shows recipes; `coop presets init` scaffolds a runnable template. On-disk
  credential storage is unchanged.
- **Loop pools are gone — rotation is the lead's model-first `models:` ladder.** `coop loop
  pool` and `pools.json` are retired; the ladder's entries are `model` or `model@account`, a
  bare model fans out across every signed-in account, and limits key per (model, account).
  Presets drop lead `model:`/`credentials:` for one `models:` list, roles drop `credentials:`
  (a role runs on its agent's default account), `--model` gains the `opus@work` shortcut, and
  the credential-first `work@opus` form plus `coop credentials <cred> model` are retired — a
  credential is just an account. Precedence: `--model`/fleet `model:` > the active ladder
  entry > `COOP_LOOP_MODEL`/preset > `COOP_<AGENT>_MODEL` > the agent default.
- **v3 keeps no compat aliases.** Retired with tombstones (exit 2 + the rewrite): `coop clone`
  (→ `coop fork`), `coop pool`/`coop loop pool`, `coop profiles` (→ `coop credentials`),
  verb-first credential edits (→ the path grammar `coop credentials <agent> <credential>
  <default|rm>`), `coop tasks start` (→ `claim`), `coop loop --debug` (→ `--debug-on-fail`).
  The forgiving spellings are gone too: `ls` and `rm` are the only list/delete verbs —
  `list`/`remove` are not accepted.
- **`coop status` is removed — `coop tasks watch` is the live board.** One task-centric view
  of the queue draining in place; with a fleet running it merges every active fork (deduped by
  task id, each in-progress task tagged by its fork). The per-fork board stays at
  `coop fleet watch`.
- **`.agent/BACKLOG.md` is retired — `coop backlog` is a task-folder drawer.** Unscheduled
  ideas live in `.agent/tasks/xx_backlog/`, outside the lifecycle, so the loop, Stop hook, and
  counters ignore them: `coop backlog add|rm|promote` — promote is a folder move into
  `00_todo/`, not a rewrite. In a monorepo it rolls up across queues like `coop tasks`.
- **`.agent/fleet.yaml` is the fleet format.** `coop fleet init`/`split` write YAML; forks can
  reference presets (`preset: frontier`) with per-fork `credential:`/`model:`/`consult:`
  overrides. The pre-v3 one-line `.agent/fleet` is not read — its presence is an error until
  translated (MIGRATING.md).
- **`coop loop` exits 3 when it stops on a blocked decision.** The exit contract: `0` verified
  done, `1` failure, `2` usage, `3` stopped with work in `50_blocked/` and nothing else
  actionable — so cron/fleet/CI can tell "drained" from "stalled on a human decision".
- **The legacy flat credential vault migrated automatically** to the per-agent
  `<agent>/profiles/<name>/` layout on first run — one layout, no manual step.

### Orchestration presets

- **A preset is the whole multi-model arrangement in one YAML file**
  (`.agent/presets/<name>/preset.yaml`): who leads (a `models:` ladder) and the roles it routes
  to — each an agent + model + routing hints in one of three modes: `native` (a Claude
  subagent, generated in-box from the role so nothing pollutes the repo's `.claude/agents`),
  `consult` (a read-only peer via `coop-consult`, role-addressed with its prompt as persona),
  or `delegate` (a write-capable `coop-delegate` worker that may edit but never commit —
  HEAD is compared before/after and runs are serialized; the lead reviews, gates, commits).
  coop generates the lead's routing contract with exact invocations and pushes real
  delegation; `coop presets init` scaffolds a self-documenting, runnable template with
  starter `roles/*.md` prompt files. `--preset` works on `coop <agent>`, `loop`, `fusion`,
  `acp`, and `coop fork --loop`; explicit flags still win. (First external dependency:
  gopkg.in/yaml.v3 — the binary stays static.)
- **Pick the model for any run:** `--model` everywhere plus `coop models` (the per-agent
  menu); `--credential <name>` picks the account for a single run; a fleet fork can pin its
  own `credential:`. `coop credentials` gained the path grammar (each token narrows down to
  one credential), flags an expired login, and deletes with
  `coop credentials <agent> <credential> rm`.
- **`coop loop` reads project-specific audit checks from `.agent/audit.md`** and appends them
  to the end-of-loop audit.

### ACP — driving coop from an editor (Zed)

- **coop owns the editor toolbar.** The proxy is always in the path: every provider runs in
  yolo mode (the box is the sandbox — coop answers `session/request_permission` itself, so the
  editor never prompts), the permission/subagent dropdowns are dropped, the model dropdown
  defaults to coop's model, and a new coop dropdown switches the credential or preset
  mid-session — transparently: the box restarts on the new identity, the handshake replays,
  and a shared session-transcript store means `session/load` finds the conversation on the new
  account. Every `coop acp` session survives a box restart (rebuild/OOM) for free;
  `--supervise` is accepted but no longer needed.
- **Rate limits are handled transparently — coop rotates (or waits) and re-sends.** A
  rate-limited turn no longer errors: coop swallows the error, suppresses the adapter's limit
  notice (only when a real limit error follows — output that merely mentions "rate limit" is
  never dropped), restarts on the next signed-in account, and re-sends your prompt; the
  toolbar dropdown moves to the credential you're now on. With every account cooling, it
  waits out the nearest reset (telling the editor when) and then re-sends. Same-provider
  only; a preset session rotates its own `models:` ladder instead.
- **Restart correctness:** the toolbar shows the restarted box's real config (the replayed
  `session/load`'s `configOptions` forward as updates); a switch before your first message
  re-creates the turn-less session instead of erroring "Session not found"; a failed reload
  warns instead of silently losing history; an editor disconnect mid-restart no longer
  orphans the new box.
- **Opt-in wire tracing:** `COOP_ACP_TRACE=1` (or the `~/.config/coop/acp-debug` sentinel,
  which works on an already-running server) appends the editor↔box wire and restart events to
  a bounded, auto-pruned `~/.config/coop/acp-trace-<pid>.log`. It holds prompts and file
  contents — treat it as sensitive.

### Monorepos

- **`.agent/project.yaml` replaces a hand-maintained `COOP_TASKS`.** List `subprojects:` and
  every task consumer — `coop tasks`, `loop`, `prompt`, `fleet` status, completion, the Stop
  hook — spans all their queues; the root keeps its own queue for cross-member work.
  `coop init` scaffolds members with their own queue, sharing the root's AGENTS.md.
- **`serve.ports` publishes a dev server in the box to your host browser** (each listed port
  maps through).

### Security & reliability

- **Hardening from the end-to-end and multi-agent audits:** secret shadowing covers
  `*.yaml`/`*.yml` credential files and matches case-insensitively (`.ENV`, `ID_RSA`);
  template suffixes (`id_rsa.example`) stay shadowed; the fork merge gate reuses the same
  shadow denylist (so `kubeconfig`, `.npmrc`, `.netrc`, `service_account.json`, `*.kdbx`
  can't land silently); `coop check-secrets` also flags `secret_key_base`-family keys,
  passwords in connection-string URLs, and `github_pat_…` tokens; `COOP_EGRESS` fails closed
  (an unrecognized value means offline, and anything but an explicit `open` forces
  `--network none`); `coop login --credential` rejects traversal/collision names; fork
  commands reject a name that escapes the forks directory.
- **Deletions confirm before they happen.** Every unrecoverable delete (`coop tasks rm`,
  `credentials rm`, `fork rm`, backlog `rm`) runs one shared gate: `--yes` proceeds, piped
  runs refuse without it, a TTY prompt defaults to No. `coop fork <name> --fresh` runs the
  same unmerged/dirty guard as `fork rm` instead of silently discarding work.
- **The loop is a better overnight citizen:** Ctrl-C finishes the current task then stops
  (a second Ctrl-C is immediate; foreground fork loops get the same); the machine is kept
  awake while it runs; "queue verified done" is only reported when the audit didn't reopen
  work; an unrelated number containing `429` no longer stalls it as a false rate limit; task
  activity shows tool paths repo-relative and flags anything outside the repo.
- **Session/setup fixes:** `coop acp --supervise` survives a box restart again; a fork (or
  `--consult`) with one signed-in agent launches with that agent instead of none; a deleted
  credential named "default" stays deleted; concurrent config edits no longer corrupt state;
  remote (HTTP/SSE) MCP servers in `mcp.json` reach every agent, not just claude.
- **Staying current:** once a day coop notes when the binary, box image, or their skew is
  stale; `coop update` refreshes agent CLI packages from npm's stable `latest`;
  `COOP_NO_ASDF=1` no longer breaks Node-based agents; `make casts` refuses to ship a
  dirty/dev version into the site's terminal casts; scaffolded `postgres:18` services start
  out of the box.

### CLI quality

- **One reference, gated:** `help.go` is the single source for the CLI reference — docs, the
  man page, and `site/llms.txt` regenerate from it and CI fails on drift; a CLI-conformance
  test graduates the repo's committed taste rules (verbs, usage placeholders, help style)
  into the gate.
- **Consistency:** `ls`/`rm` verbs and one usage-placeholder lexicon everywhere; grammar
  parity across launch paths (`coop loop --credential` works like `coop <agent>
  --credential`); split slices are named consistently; typo suggestions now catch 3-rune
  slips; a bare command group prints its help, never an `unknown command ""` error (bare
  `coop tasks` lists the queue).
- **Output:** stdout stays machine-clean when piped (color and decoration gate on a TTY);
  task and decision lists render in color with breathing room; progress bars show blocked
  work in red; the live watch view no longer garbles; errors name the fix (`coop up` with no
  compose points at `coop init --services`); `coop doctor` warns when probing an alpine
  stand-in instead of the real box image; `coop help` opens with a FIRST RUN line for a
  fresh machine.
- **Quality of life:** `coop prompt` prints a one-line repo status for your shell prompt or
  tmux (non-zero segments only, read-only and cheap); `coop completion bash|zsh` ships shell
  completions; `coop tasks add` fills a task inline (`--context/--acceptance/--approach/
  --subtask`); `:d` in `coop tasks decisions -i` deletes the current decision's task (see
  Unreleased); a new `/review-board`
  skill convenes the heavyweight pre-merge review; `coop init` scaffolds the orchestrator
  pattern and guidance; filesystem-only commands no longer demand a container runtime; a
  fresh fork's `coop fork ls`/`review` no longer shows inherited state as fork activity.


## 2.10.1

- **The loop continues an interrupted task instead of stranding it `[w]`.** When an iteration was
  killed mid-task — a rate limit, crash, or timeout after the agent claimed `[w]` but before it
  committed — the claim used to sit `[w]` forever: later iterations only picked up `[ ]` items, and if
  the stuck task was the last one the loop exited reporting "✓ done". Now the loop keeps running while
  any `[ ]` **or** `[w]` remains, and the work prompt tells the agent a `[w]` is an interrupted attempt
  whose partial work may be uncommitted — to inspect `git status`/`git diff` and continue it (or
  discard and redo if it's off-track) before starting new `[ ]` work. A stall guard stops the run if
  no task completes for several iterations, so a genuinely unfinishable task can't spin forever. This
  pairs with the loop's existing profile rotation (`coop pool`): a rate limit switches subscriptions
  and the new profile continues the same task from its on-disk partial work.

## 2.10.0

- **Every box auto-starts the repo's sibling services (`compose.agent.yml`).** Previously only the
  explicit `coop up` brought services up; a box merely *joined* their network if it already existed,
  so launching `coop fusion` / `coop acp` / `coop loop` / a fork without remembering `coop up` first
  left the agent unable to reach its db/redis. Now every launch path funnels through `box.Run`, which
  runs `compose up -d --wait` before starting the box whenever a compose file is present — so services
  are up and healthy for any mode. It's idempotent (already-running services are a fast no-op), gated
  the same way as the network join (skipped offline via `COOP_EGRESS=none`, on the Apple `container`
  runtime which has no compose, or with `COOP_NETWORK=0`), and never blocks the session — a failure
  warns and continues. Opt out with `COOP_AUTO_UP=0` to keep managing services with `coop up`/`coop down`.

## 2.9.0

- **A `claude` peer consult no longer hangs forever (`coop-consult` detaches peer stdin).** `claude -p`
  reads piped stdin *in addition to* its prompt argument, so when the lead backgrounded a consult the
  peer inherited an open pipe and blocked reading it until killed — a `claude` consult timed out at the
  status line while `gemini` returned. The wrapper now captures the prompt up front and detaches stdin
  (`exec </dev/null`) for every peer, so none can block on it (codex's per-call redirect folds into the
  one shared redirect).

- **Each `coop-consult` is time-bounded so a slow or wedged peer can't stall the lead.** Every peer
  call now runs under a timeout (default **30 minutes**); on expiry the peer is skipped with a clear
  notice so the lead synthesizes from whoever answered, instead of the consult blocking and the lead
  hand-rolling its own `timeout`. Set `COOP_CONSULT_TIMEOUT` (seconds) — via env or `coop.conf`, now
  forwarded into the box — to tune it per run.

## 2.8.0

- **`coop loop --preflight` runs a cleanup pass before working the queue.** Opt-in (`--preflight`, or
  `COOP_PREFLIGHT=1` to default it on; `--no-preflight` overrides). Before the first work iteration it
  runs one agent pass that compacts `.agent/LOG.md`, removes done `[x]` tasks already committed, and
  unblocks any `[B]` item whose `.agent/PENDING_DECISIONS.md` entry now has an answer — so a fresh run
  starts from a tidy queue. It works no task and makes no commits (these files are git-ignored); it's
  the symmetric front bookend to the existing audit pass, and is skipped under `COOP_LOOP_CMD`.

- **The box repairs a bare `node` when an orphaned asdf nodejs shim shadows the image's node.** After
  you run coop on a repo that pins `nodejs` in `.tool-versions`, asdf keeps that node shim in the
  persisted `~/.asdf` volume. In a later repo that doesn't pin nodejs (e.g. a Go project), the shim
  shadowed the image's node and failed with "No version is set for command node" — which broke the
  Node-based agent CLIs invoked as subprocesses, so `coop fusion` / `--consult` peers (and any node
  tool an agent shelled out to) errored even though the lead agent itself ran. The box entrypoint now
  detects a broken `node` and pins the newest installed asdf nodejs as the global fallback; a repo's
  own `.tool-versions` still overrides it. Run `coop build` to pick it up.

- **Fusion/`--consult` peers can keep their thread across turns (`coop-consult` wrapper).** The
  governor/lead now consult peers through a small mounted `coop-consult <peer> --fresh|--continue`
  script instead of raw `claude -p …`/`codex exec …`/`gemini -p …`. `--fresh` starts a new read-only
  session; `--continue` resumes the peer's *own* prior consult, so a follow-up sends only the delta
  instead of re-pasting the static context. It hides the per-agent session-id mechanics (claude/gemini
  start under a generated `--session-id`, codex's `thread_id` is captured from `exec --json`) and
  prints whether it continued or fell back to fresh, so the lead knows when to resend full context.
  `--consult` defaults to `--fresh` (independent second opinions); fusion's instruction tells the
  governor to continue within a subject and start fresh on a new one. Peers stay read-only throughout.

- **Fork re-entry resumes the right session, even after a loop or consult ran in the fork.** Re-entry
  used to resume the *most-recent* session for the fork's directory (claude `--continue`, gemini
  `--resume latest`), which a `coop fork … --loop` iteration or a fusion/`--consult` peer call sharing
  that directory could win — landing you in the wrong thread. coop now assigns claude/gemini forks
  their own session id (stored in the fork's git-excluded `.coop/`) and resumes exactly it; codex,
  which can't be handed an id, resumes the most-recent *interactive* session and skips `codex exec`
  loop/consult runs. This also sidesteps gemini's basename-keyed session store (same-named forks in
  different repos no longer collide on resume).

- **Fusion governor consults peers with self-contained prompts, not your message verbatim.** The
  governor is now told that peers are read-only advisors — it consults them on the *thinking* a task
  needs and makes every change itself — and that each consult is a fresh, memoryless call, so it
  composes a self-contained prompt (goal + context + question) rather than forwarding your message
  verbatim, which was meaningless to a peer past the first turn (e.g. "fix the second one").

- **`coop update` now self-updates the `coop` binary, then rebuilds the box image.** Previously it
  only refreshed the box (base image + agent CLIs + ACP adapters), leaving the binary pinned at
  whatever you installed. It now first upgrades `coop` to the latest GitHub release — fetched and
  verified the same way `install.sh` does (checksum + cosign), and swapped in atomically (rename, not
  in-place write) so replacing the running binary is safe — then rebuilds the image. `--self-only`
  updates just the binary; `--box-only` keeps the old box-only behavior. A dev/source build, an
  already-current binary, or a coop installed somewhere unwritable (a package-manager prefix) skips
  the self-update with a note; an offline check is a soft warning that still lets the box rebuild.

## 2.7.2

- **A crashed fork whose PID gets reused is no longer mistaken for "still running".** Liveness only
  checked that the recorded PID existed, so after a worker crashed and the OS reused its PID for an
  unrelated process, the fork read as running — blocking `coop fleet up` / `coop fork merge` /
  `coop fleet prune` on it. The pidfile now records the worker's start time, and a PID whose start
  time no longer matches is treated as dead (a missing/uncheckable start time stays "alive", so a
  genuinely live loop is never wrongly double-started). Old pid-only pidfiles still work.

## 2.7.1

- **`coop fleet up` is loud when it aborts partway.** It still fails fast on the first fork that
  can't start (a silent partial fleet discovered hours later is worse), but the error now says how
  many forks already started and how to clean them up (`coop fleet down`), instead of only naming the
  fork that failed.

- **`coop fleet watch` and the loop progress bar ride out a torn `TASKS.md` read.** The agent
  rewrites its queue as it works; a refresh landing mid-rewrite could read the file empty for an
  instant and flash a wrong "0/0" / "(no queue)". A populated queue never legitimately empties, so a
  zero-task read now keeps the last good counts.

- **`coop fork merge` refuses to land a fork whose loop is still running.** Merging rebases inside
  the fork's worktree and then deletes it; doing that to a fork whose detached loop is mid-iteration
  corrupted the in-flight work and orphaned the worker. `coop fork merge <name>` now errors if that
  fork is still running (stop it first), and `coop fork merge --all` skips still-running forks with a
  notice and lands the rest — matching how `coop fleet prune` already guards.

- **`.agent/fleet` rejects a misspelled agent, a path with spaces, and duplicate fork names.** A line
  like `api borg .agent/TASKS.api.md` silently treated `borg` as the path (dropping the real one),
  surfacing later as a baffling "no such file: borg"; a path with spaces was truncated to its first
  word; and a repeated fork name was accepted, silently dropping the second. These are now parse-time
  errors that name the problem.

- **The loop names the task queue (and `AGENTS.md`) as absolute paths, so Gemini/Codex fleet forks
  can read it.** Each iteration told the agent to `Read .agent/TASKS.md …` — a repo-relative path.
  Claude/Codex resolve it against the box's working dir, but Gemini's `read_file` rejects a relative
  path outright, so a Gemini (or Codex) fork in a fleet couldn't read its own queue and stalled at
  0/N. The prompt now names absolute in-box paths.

- **`coop fleet watch`: a fork with only blocked tasks left reads as "blocked", not "✓ done".** The
  row used "no actionable task" as the done signal, but `scanTasks` reports that for an all-`[B]`
  queue exactly as for an all-done one — so a fork at 2/5 with 3 blocked (even while still running)
  flashed "✓ done". "✓ done" now requires every task to be `[x]`; an unfinished fork with nothing
  actionable shows "blocked".

- **`coop fleet watch` shows a stopped fork as "stopped", not "paused".** A fork whose loop had
  exited with tasks still unchecked was painted with the idle `‖` glyph and its *next* task name, so
  a fork that quit at 0/20 read as "paused, working on Task 1". Such a fork now shows a yellow mark
  and "stopped" and stays legible; only a fork that never started (no log) recedes. (Done forks are
  still `✓ done`, running forks still spin.)

- **`coop fleet watch` no longer spams its header into scrollback.** The live dashboard repainted a
  bottom-pinned region, which redraws in place by counting lines up from the bottom — but once the
  dashboard was taller than the terminal pane, every refresh scrolled the top line (`coop fleet — N
  running…`) into scrollback instead of overwriting it, leaving a growing wall of repeated headers.
  It now renders on the alternate screen buffer (like `top`/`htop`): each frame repaints from the
  top-left and the prior screen is restored on exit, so it never pollutes scrollback regardless of
  window size.

## 2.7.0

- **`.coopignore` can re-hide a whitelisted template / CA-bundle name.** AllowGlobs (`*.example`,
  `*.sample`, `*.template`, `cacerts.pem` and friends) used to win over *both* the built-in secret
  patterns and an explicit `.coopignore`, so a name like `cacerts.pem` or `.env.example` stayed
  visible even when you listed it in `.coopignore`. AllowGlobs now overrides only the built-in
  false positives; an explicit `.coopignore` entry is authoritative and re-hides the file. Defaults
  are unchanged — templates and public CA bundles still stay visible unless you opt to hide one.

- **Value-bearing CLI flags reject a missing value or stray argument instead of silently doing the
  wrong thing.** `coop login claude --profile` (no name) used to fall back to the default profile;
  `coop login claude extra`, `coop init --bogus`, and a trailing `coop tasks --tasks` were silently
  ignored. They now exit 2 with a message naming the missing value or unknown flag. A shared
  `flagValue` parser handles `--flag value` and `--flag=value` for `--profile`, `--stack`,
  `--services`, and `--tasks`; agent pass-through flags are unaffected.

- **`coop acp --supervise` teardown and replay are hardened.** Three narrow lifecycle bugs in the
  supervise/restart path are fixed: (1) tearing down during the startup window (after `docker run`
  begins but before the box is labelled) could orphan the container — the inner `coop acp` now runs
  in its own process group (killed as a group, so its `docker run` grandchild dies too) and the box
  gets a deterministic `--cidfile`, so the supervisor removes it by id even before labels exist;
  (2) replay no longer blocks forever on a hung restarted agent — it waits generously for the first
  response (a cold box may be provisioning) then bounds the gap between responses, failing in-flight
  requests and giving up cleanly instead of freezing the editor; (3) a `session/new` in flight when
  the child died no longer leaks its pending-request entry. `--supervise` is unchanged for users.

- **Task-queue contract now matches the parser: every top-level `[ ]` is live.** The docs said
  the loop works `[ ]` items "under `## Active`", but the loop/split/status act on every
  unchecked top-level task wherever it sits. AGENTS.md, the scaffolded `coop init` templates,
  README, and `coop tasks lint`'s wording now state the real rule — `## Active` is convention,
  the example uses `[E]`, and anything not ready to work belongs in `BACKLOG.md`/`IDEAS.md`.

- **`install.sh` fails closed when `checksums.txt` lacks the selected asset.** Previously, if
  the checksum file was fetched but had no line for `coop_<ver>_<os>_<arch>.tar.gz`, the
  extracted checksum was empty and the mismatch check was skipped — an unverified binary
  installed silently. A missing entry is now treated as a release-integrity failure and aborts.
  The verification is factored into a `verify_checksum` helper (unit-tested without network);
  cosign signature verification is unchanged.

- **`coop check-secrets` is honest about gitignored files, and `--include-ignored` scans them.**
  The default scan covers the commit-candidate files (tracked + untracked, gitignored excluded)
  — but a `coop run`/`shell`/`loop` bind-mounts the *whole* working tree, so a
  gitignored-but-not-shadowed file (a stray `serviceAccount.json`, say) is still visible to the
  agent even though the old docs implied the scan saw "everything not shadowed". The output now
  names which scope ran, and `coop check-secrets --include-ignored` walks the full visible tree
  (still skipping shadowed files, dependency/build dirs, and binaries). README + help updated.

- **A run now mounts only the launched agent's credentials, not every agent's.** A plain
  `coop claude` used to bind-mount `~/.claude`, `~/.codex`, *and* `~/.gemini` (and pass the
  whole `agents/env` with every API key) into the box, so the Claude process could read the
  Codex/Gemini logins it had no need for. Each run is now scoped: a plain agent run mounts just
  its own home and API key; `coop fusion` and `coop <agent> --consult` (and forks) also mount
  the *authenticated* peers they're told to consult; raw `coop run`/`coop shell`, the merge
  gate, and `coop doctor` mount no agent credentials; `coop login <agent>` mounts only the agent
  being signed in. Peer API keys are filtered out of the env file for scoped runs; non-key
  runtime vars still reach every box. `COOP_HOMES=0` still disables homes entirely.

- **`.tool-versions` toolchains (go, ruby, …) now resolve in a login shell too.** The box puts
  asdf's shims on PATH via the image `ENV`, which only reaches the agent process and non-login
  shells; a login shell (`sh -lc`, `bash -l`) sources `/etc/profile`, which resets PATH to the
  Debian default and dropped the shims — so a gate run through a profile-sourcing shell saw
  `go: not found` even though go was installed and shimmed (node/python/git survived, living in
  `/usr/local/bin`). The base box now ships an `/etc/profile.d/asdf.sh` drop-in that re-adds the
  shims for login shells. Rebuild with `coop build` to pick it up.

- **`coop acp --supervise` no longer drops a request when the box restarts.** On a restart the
  proxy failed the in-flight requests (telling the editor to retry) a beat *before* it repointed
  to the new child, so a retry that arrived in that window was written to the dead child and
  silently dropped (a tight timing race; it also surfaced as an intermittently deadlocking test).
  The new child is now published before the retry signal, so the retry lands on it.

- **`coop loop` shows which model each iteration is running.** When Claude streams its activity on a
  TTY, the loop now surfaces the model from the agent's `init` event (e.g. `· model claude-opus-4-8`)
  right after the iteration banner — so a long unattended run shows the model actually working,
  including when credential-pool failover rotates to a different account or tier. coop doesn't choose
  the model, so this reflects the agent's own report; non-streaming agents and detached fork logs are
  unchanged.

## 2.6.1

- **`coop doctor` works on rootful Linux Docker.** Its self-test probe was created mode 0600 and
  its fixture 0700; 2.6.0's new `--cap-drop ALL` strips `CAP_DAC_OVERRIDE` from the probe's
  root (alpine) container, so on rootful Docker it could no longer read its own probe and
  reported "the sandbox produced no output" (rootless podman and Docker Desktop remap ownership,
  so they were fine). The probe and fixture are now world-readable, and the check surfaces the
  container's actual error on failure. Real `coop run` was never affected — the box runs as
  non-root `node`, which never holds the capability either way.

## 2.6.0

- **`coop build` / `coop update` transparently restart supervised editor sessions.** They
  used to SIGKILL every running box (dropping a live editor session — Zed showed "Server
  exited with status 137"). Now they restart only **supervised** sessions (`coop acp
  --supervise`, tagged `coop.supervised`) onto the new image — the supervisor reconnects and
  replays the ACP handshake, so the editor doesn't notice — and leave everything else (a
  loop, a fork, an un-supervised session) running on the old image until it next starts.
  So after you edit `Dockerfile.agent` or `.tool-versions`, `coop build` moves your editor
  onto the rebuilt box without resetting the session. (The old `--restart` flag is gone.)
- **`coop acp --supervise` survives a box restart without the editor noticing.** Editors
  keep one ACP server process per agent and don't respawn a crashed one until you restart
  the whole editor. With `--supervise`, `coop acp` runs the agent in a child and proxies
  the connection; if the container dies (a rebuild, OOM, Docker restart), it starts a new
  one and replays the ACP handshake (`initialize`, `authenticate`, `session/load`), so the
  editor stays connected and the conversation resumes from the mounted home (verified
  end-to-end against the real claude + codex adapters: kill the box mid-session and the
  next prompt succeeds, still authenticated). Each supervisor tags its box `coop.sup=<id>`
  and kills exactly its own box on teardown — not other agents' supervised boxes. Opt-in; set
  it in your editor's args, e.g. `["acp","claude","--supervise"]`.
- **`coop init` scaffolds a commit gate that matches the repo's stack.** The pre-commit
  hook (and the Claude commit gate) used to hardcode a `gofmt` check — so a Terraform or
  Elixir repo got a dead Go gate and no gate for the language it actually uses. Now `coop
  init` detects the stack from go.mod / `*.tf` / mix.exs / Cargo.toml (or `.tool-versions`)
  and generates the matching format check: `gofmt`, `terraform fmt`, `mix format`, or
  `cargo fmt` — each `command -v`-guarded so it runs in the box and skips where the tool
  is absent. If it can't detect anything it leaves the gate **neutral** (no checks
  imposed) rather than guessing; at a terminal it asks which gate to add.
- **`coop init` suggests building the box on the repo's existing Docker.** When the repo
  already has a Dockerfile or compose file (and no `Dockerfile.agent` yet), `coop init`
  prints how to base the agent box on your image — the coop agent layer (node + the agent
  CLIs + a `node` user) to add on top — and how to reuse the compose services for `coop
  up`. Docs only: it lists the Dockerfiles, the compose services it found, and a ready
  snippet, but writes nothing.
- **Sibling services are opt-in.** `coop init` no longer drops a `compose.agent.yml`
  (Postgres + Redis) into every `.tool-versions` repo. At a terminal it asks which to add
  — none by default — or pass `--services postgres,redis`; a project that doesn't want a
  database isn't handed one. The file it writes carries only the services you picked.
- **`coop init` seeds an empty `mcp.json`.** It writes an empty
  `~/.config/coop/agents/mcp.json` (the shared MCP source of truth) so there's an obvious,
  correctly-shaped file to declare servers in — no more hunting for the path or format. An
  empty (no-server) `mcp.json` is now **inactive**, so the stub is a pure no-op until you
  add a server: an existing config is never touched, and runs are unchanged until you fill
  it in.
- **`coop init` output reads as a log plus next steps, not a wall of `coop:`.** The per-file
  progress (wrote/linked/added/gate) is now faint and unprefixed, a single `coop:` line
  summarizes, and the actions you take next — `coop build`, `coop up`, `coop loop`, each shown
  only when it applies — stand in their own spaced, colored "next steps" block. No more hunting
  for the three lines that matter among the twenty-five that don't.
- **Playwright works in the box.** Chromium's system libraries are now baked into the base
  image as root — the part an agent, running as the non-root `node` user, can't `apt-get`. The
  browser binary downloads to the cached `~/.cache` volume on first use, and the bundled
  `@playwright/mcp` example runs `--headless --no-sandbox` (the box is already the sandbox). So
  `npx playwright install chromium` + a `{ args: ['--no-sandbox'] }` launch — or the MCP server
  — drives a browser and takes screenshots instead of failing on a missing `.so`.

- **A live progress bar.** On a terminal, `coop loop` pins a Docker-build-style status bar to
  the bottom of the screen while it runs — a spinner, a progress bar, the done/total task count,
  the task in flight, and elapsed time — and the agent's activity scrolls above it. The bar
  tracks the queue as the agent rewrites it, so progress moves live within an iteration, and the
  run is bracketed with starting and finishing tallies. Piped or CI output stays plain.
- **Watch the agent work.** On a terminal, `coop loop` with Claude renders the agent's activity
  live instead of going dark until the iteration ends — each tool call (`✎ Edit`, `⚙ Bash`,
  `▸ Read`), the agent's own narration (marked `✦`), and a closing `· N turns · time · $cost` —
  by decoding Claude's stream-json output. coop's own lines wear a bold-cyan `coop:` so its
  voice stays distinct from the agent's in the scroll. Multi-subscription failover keeps working underneath (the
  structured rate-limit signal is translated for the detector). Other agents, a custom
  `COOP_LOOP_CMD`, and piped/CI runs keep plain text output.
- **Watch the whole fleet.** `coop fleet watch` (or `coop status --watch`) turns the fleet
  roll-up into a live dashboard — one row per fork with a spinner, a progress bar, the task in
  flight, and its last log line, plus a global progress bar across every fork's tasks — refreshing
  in place. It polls the same queue/log/pidfiles `coop status` reads (no daemon); Ctrl-C exits.
  A non-terminal falls back to a single `coop status` snapshot.
- **Multi-subscription failover for the loop.** One agent can now hold several accounts
  as named profiles — `coop login claude --profile work` — and when the unattended loop
  hits a rate/usage limit it switches to another signed-in profile and keeps going, only
  waiting once every profile is limited. So a long run rides through a (multi-day)
  subscription cap instead of parking on it. With no setup it rotates across all of an
  agent's signed-in profiles; `coop pool add <agent> <profile…>` narrows a repo to a
  chosen set, and `coop profiles` lists what you have. `coop profiles default <agent>
  <name>` marks which profile an interactive `coop claude` uses, so the default is a
  mark you set rather than whichever profile happens to be named "default". Profiles live
  in the vault
  (`~/.config/coop/agents/<agent>/profiles/`), never in the repo, and only the active one
  is mounted into the box — a running agent sees just the account it's using, not the
  whole vault. Existing single logins are untouched: they become the "default" profile in
  place, with no migration, and behave exactly as before.
- **Configurable, multi-file task queues.** `coop tasks` and `coop loop` take a repeatable
  `--tasks <path>` (or `COOP_TASKS`, space-separated), defaulting to `.agent/TASKS.md`. A
  monorepo with a queue per component can now inspect or drain several at once: `coop tasks
  list --tasks portal/.agent/TASKS.md --tasks runner/.agent/TASKS.md` aggregates them under
  per-file headers, and `coop loop --tasks portal/.agent/TASKS.md --tasks runner/.agent/
  TASKS.md` works the union until every file is drained — one loop covering several
  components, with the whole repo still mounted. list and lint span all the files; add and
  split target a single one. Paths are relative to the repo root.
- **Host-side git is hardened against a poisoned repo.** coop bind-mounts your repo into the
  box with its `.git` writable, so a prompt-injected agent could plant git config that runs a
  command on *your* host the next time coop touches the repo — a `core.fsmonitor`, a hook,
  `diff.external`, a `gpg.program`, a filter/merge/diff driver. Every host-side git call coop
  makes now blanks those exec-bearing knobs, and any config coop reads then executes or reads a
  host file from (your editor, signing program, excludesfile) is read from your **global** git
  config, never the agent-writable repo. `coop check-secrets`, `coop fork merge`/`review`, and
  `coop status` are all covered, each with a poisoned-config test that fires a raw-git positive
  control so the guard can't silently rot.
- **The box drops privileges and can run fully offline.** Every box now starts with `--cap-drop
  ALL` and `no-new-privileges` (an agent needs neither), so a repo `Dockerfile.agent` that does
  `USER root` can't regain `NET_RAW`/`MKNOD`/etc. New opt-in `COOP_EGRESS=none` runs the box
  with no network at all — for a run you don't trust. The secret-shadow denylist gained common
  service-credential names, and the README now states plainly that **`.coopignore`, not
  `.gitignore`, is the boundary** for what the agent can read on a bind-mounted run.
- **`coop fork merge` defends the host on land.** Landing a fork runs git on your machine, so a
  fork that planted an execution-on-interaction file (`.envrc`, `.vscode/tasks.json`, a new
  `Makefile`, a `package.json` that adds an install script) or neutralized its `.gitattributes`
  drivers is flagged and blocked without `--force`; an untracked `Dockerfile.agent` (which
  defines the box an agent could author) is flagged before `coop build`. And `coop fork merge
  --all` now asks before it lands and **deletes** every fork — it used to do that with no
  confirmation at a terminal.
- **CLI papercuts, fixed.** `coop run --help` and bare `coop run` print usage instead of
  crashing the box; `coop login` requires the agent and refuses a non-interactive stdin; `coop
  help <cmd>` shows that command's page; every command has a real `--help`; a bad
  subcommand/agent/flag is rejected the same way everywhere, with a "did you mean…?";
  `$COOP_RUNTIME` is validated up front (a clear "runtime not found", not a misleading "image
  not built"); and the `coop status` / `coop fork ls` / `coop profiles` tables size their
  columns to the data, so a long fork name no longer breaks the alignment.
- **Safer fork loops.** `coop fork` refuses to start a second loop on a fork that's already
  looping (it was overwriting the pidfile, orphaning the first worker and leaving two loops
  racing the same worktree), and `coop fleet up` skips already-running forks so a re-run is
  idempotent. An empty `COOP_CLAUDE_CMD` / `COOP_GEMINI_CMD` override now degrades to the bare
  CLI instead of producing a command with no executable.

## 2.5.2

- **`coop acp` runs quiet.** ACP speaks to an editor over stdio, so coop's progress lines
  (the secret-shadow count, the fusion-governor note) and the box's toolchain-provisioning
  chatter were just noise in the editor's log. ACP now suppresses them — the toolchain
  still provisions, silently (`COOP_QUIET`). Other modes are unchanged.
- **Consistent `coop:` log prefix.** coop's progress/error lines used a dimmed `agent:`
  prefix while the box's provisioning printed `coop:` — now both are a dimmed `coop:` (the
  tool's own name), so output reads as one voice.
- **The box only narrates provisioning when there's actually something to install.** The
  "provisioning toolchain…" line (and asdf's "already installed" output) printed on every
  launch even when every pinned tool was already present — pure noise. The entrypoint now
  checks `.tool-versions` against what's installed and stays silent unless a tool is
  missing.
- **`coop check-secrets` no longer scans vendored or gitignored files.** It enumerates the
  working tree the way git does — tracked plus untracked, gitignored paths excluded —
  instead of walking everything. Build output and dependencies (`node_modules/`, `dist/`,
  `_build/`) are skipped, which is where the bulk of the false noise lived: across the
  author's projects one app dropped from ~1,900 hits to a handful.
- **The secret scanner flags literal credentials, not references to them.** The entropy
  heuristic was tripping on ordinary code and config, not just secrets. It now requires the
  key to *end* in a credential word (so `authenticator`, `token_url`, `allocate_tokens`
  no longer match) and skips values that are references rather than literals: variable and
  config refs, `${…}`/`{{…}}` interpolations, function calls, Rust generics/namespaces,
  Elixir module attributes, `SCREAMING_SNAKE` constants, comments, URLs and paths, and
  obvious placeholders or fixtures (AWS `…EXAMPLE` keys, `"very-long-password-1234"`). A
  real random token contains none of these, so a genuinely committed secret still surfaces.
  Shared by `coop check-secrets` and the `coop fork merge` policy.

## 2.5.1

- **Secret directories are now shadowed on Podman too.** Directory shadowing used
  `--tmpfs`, which Podman applies in a separate pass from `-v` binds — so the repo bind
  re-covered it and `secrets/`-style directories were re-exposed inside the box (file
  shadowing via the read-only decoy was unaffected, and Docker was unaffected because it
  sorts all mounts by destination). Now a directory is shadowed by a read-only empty-dir
  bind, which sorts with the repo bind on every runtime. `coop doctor` passes on Podman.

## 2.5.0

- **`coop check-secrets` — content secret scan of the visible tree.** Shadowing hides
  secrets by filename; this catches a token hiding *inside* an ordinary file. It walks the
  non-shadowed working tree (exactly what the box can see), runs the same content scanner
  the fork-merge policy uses, and reports each as `file:line`, exiting non-zero on a hit
  (pre-flight / CI). The shadow rule is now one shared `box.NewShadowDecider`, so the
  scanner and the mount plan can't disagree about what the box sees.
- **`coop tasks` — `.agent/TASKS.md` as a validated surface.** `list` shows states and
  titles, `lint` flags stale `[w]` claims / tasks missing the self-contained five-part
  shape / unchecked tasks stranded in `## Example` / malformed markers (exit 1 on
  findings, for pre-flight or CI), `add "<title>"` appends a well-shaped stub, and
  `split <n>` carves the queue into slices. All run through one anchored parser shared
  with the loop, fleet split, and status — so `coop fleet split` now carries each task's
  whole body into its slice (slices stay self-contained) instead of bare title lines.
- **`coop fleet init`.** Writes a documented `.agent/fleet` template — the
  `<name> [agent] <tasks-path>` format explained in inline comments — so you can declare a
  fleet without looking up the syntax. Refuses to clobber an existing file.
- **`coop status` + a richer `coop fork ls`.** Watching a fleet loop overnight meant
  tailing N logs. `coop status` now rolls the fleet up at a glance — per fork: running or
  idle, tasks done/total, blockers, diff size, and the task it's on — plus fleet totals.
  `coop fork ls` gains a tasks-progress column. Both read existing sources (the fork's
  queue, git, the loop pidfile); no daemon. The anchored TASKS.md parser is shared, so the
  loop, fleet split, and status can't drift apart.
- **Aligned `coop fork ls` / `coop status` tables.** Bold column headers no longer count
  their (invisible) ANSI escape bytes toward the `%-Ns` column width, so on a terminal the
  header lines up with the rows beneath it instead of drifting left.
- **Unknown commands fail fast instead of being run in the box.** `coop <typo>` used to be
  shipped into the box and die with a cryptic `exec: …: not found` after a slow toolchain
  spin-up. An unrecognized command now errors immediately with a "did you mean …?"
  suggestion and a reminder that raw box commands are explicit. This drops the implicit
  `coop npm test` passthrough — run raw commands with `coop run -- npm test`.
- **`coop <command> --help` shows that command's own help.** Every subcommand's `--help`
  used to print the generic usage; now it prints focused help — synopsis, what it does,
  and its flags/subcommands. (`coop fork` keeps its fuller help; `coop run` and the agents
  still forward `--help` to the underlying command.)
- **`coop <command> help` works, and no-argument commands reject stray args.** `coop build
  help` (or any extra token) used to be silently ignored — the build just ran. Now a
  positional `help` prints the command's help like `--help`, and the no-argument commands
  (`build`, `update`, `doctor`, `status`, `up`, `shell`, `check-secrets`) reject unexpected
  arguments with a clear error instead of ignoring them.
- **`coop help` is a clean command reference.** Reworked into one-command-per-line groups
  (fork verbs are listed individually, not collapsed into `<verb>`), fixed long commands
  gluing onto their descriptions, and dropped the verbose prose tutorials — each command's
  flags and examples now live in its own `coop <cmd> --help`.
- **`coop init` installs a git pre-commit gate for every committer.** The scaffolded
  `.claude/hooks` only fire for Claude, so Codex/Gemini and plain `git commit` bypassed
  the format gate. `init` now also writes a tracked `.githooks/pre-commit` (gofmt-checks
  staged files, fails closed) and sets `core.hooksPath=.githooks`, so the gate runs for
  everyone and travels with a fresh clone. A custom `core.hooksPath` is never clobbered;
  `git commit --no-verify` skips it.
- **Stale-image warning.** A per-project image (built from `Dockerfile.agent`) bakes the
  repo's toolchain at build time, so it can drift from the files that define it. `coop`
  now records a hash of `Dockerfile.agent` + `.tool-versions` at `coop build` time and, on
  a later interactive run, warns when they've changed so you remember to rebuild. (The
  shared base is exempt — `coop update` keeps it fresh.)
- **`coop loop --debug-on-fail`.** When an iteration fails at a terminal, the loop opens
  an interactive shell in the box (same repo + image) instead of auto-retrying or
  stopping — inspect files/env/run the gate, then exit the shell to retry (Ctrl-C to
  stop). A no-op in non-interactive/detached runs.
- **`.coopignore` works in subdirectories.** Like `.gitignore`, a `.coopignore` in any
  directory is scoped to that subtree (basename patterns at any depth under it, path
  patterns relative to it), so a monorepo's sub-teams can keep folder-local shadow rules
  next to their code instead of all in the repo root.
- **`coop fork merge` scans changed content for secrets, not just filenames.** The
  policy check now reads each changed blob and flags real credentials — provider token
  shapes (AWS/OpenAI/Anthropic/GitHub/Slack/Google/Stripe, private keys) and high-entropy
  values on secret-named keys — so a token committed inside an ordinary `config/prod.yml`
  is caught even though its filename is innocuous (override with `--force`).
- **The box caps a runaway agent.** Runs now set a `--pids-limit` (default 4096, a
  fork-bomb cap) and `--security-opt no-new-privileges`, with optional `--memory` /
  `--cpus`, so an agent in a loop can't fork-bomb or starve the host. Tunable via
  `COOP_PIDS` / `COOP_MEMORY` / `COOP_CPUS` / `COOP_NO_NEW_PRIVILEGES`. Applied on docker
  and podman; Apple's `container` CLI differs, so it's skipped there for now.
- **`coop fleet split` no longer creates phantom tasks.** It slices only real `- [ ]`
  task lines (the same anchored rule the loop uses), so the TASKS.md legend or an
  `## Example` block can't become a fake item in a fork's queue.

## 2.4.0

- **Fusion mode consults on every task.** The governor's directive is now
  unconditional — no "trivial change" or "I already know it" exception — so a
  fusion governor always consults both peers before answering or acting (only
  incidental shell like `ls`/`git status` is exempt, as it isn't itself a task).
- **In-box agents no longer trip over the absent OS sandbox.** The box is itself the
  sandbox and ships no bubblewrap, so coop tells Claude Code to skip subprocess
  env-scrubbing (`CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0`) and pins its bash sandbox off
  in settings (`sandbox.enabled=false`, `failIfUnavailable=false`); the shared
  `INSTRUCTIONS.md` now notes a missing sandbox is expected, not a bug. Codex and
  Gemini already launch unsandboxed (`--dangerously-bypass-approvals-and-sandbox`,
  `--yolo`), so they needed no change.
- **`coop clone` is now `coop fork` — a full local-PR lifecycle, not a one-shot
  handoff.** A fork is a throwaway local clone (own `origin`, nowhere to push, no
  gitignored secrets); treat it like a contractor's PR — open, review, merge, close:
  - `coop fork <name> [claude|codex|gemini]` — open or re-enter a fork and run the
    chosen agent. **A fork remembers its agent** (persisted git-excluded inside the
    fork), so re-entering without one continues with the model it was created with, not a
    silent fallback to claude. **Re-entering also continues your last session by default**
    (the fork's history persists): claude `--continue`, gemini `--resume latest`, and
    codex by the session whose recorded `cwd` is that fork — each **scoped to this fork**
    so it never resumes an unrelated session. Falls back to fresh when none exists;
    `--new` forces a fresh session.
  - `coop fork ls` — list this repo's forks with agent, branch, change size, last activity;
    `coop fork open <name>` opens it in your editor, `coop fork path <name>` prints its path.
  - `coop fork review <name>` — fetch the fork's branch into `review/<name>` and show
    the diff (no more hand-typed `git fetch … && git diff`).
  - `coop fork merge <name>` — land it by **rebasing** the fork onto your branch
    (linear history, no merge commit), then offer to close it; refuses if your tree
    is dirty.
  - `coop fork rm <name>` — discard a fork; refuses while its work is unmerged or
    dirty unless `--force`.
  Forks live in a sibling `../<repo>-forks/` (was `-agents/`). `coop clone` stays a
  back-compat alias. **`coop dispatch` is removed** — it was a single fork with an
  implicit `TASKS.<name>.md` mapping, now fully covered by
  `coop fork <name> <agent> --loop --tasks <path>`.
- **A fleet of forks, each on a different model, looping in the background.**
  `coop fork <name> <agent> --loop --tasks <path>` runs the unattended loop in a fork
  with the chosen model — claude (`-p`), codex (`exec`), or gemini (`-p`) — seeding its
  queue from the tasks file you name. `--tasks` is **required and explicit** (no implicit
  `TASKS.<name>.md` mapping), so a fork and its tasks file are named independently. Add
  `-d` to detach it (session-leader background worker; logs captured to
  `../<repo>-forks/.coop/<name>.log`). New process commands: `coop fork logs [name] [-f]`
  (no name = every fork at once, prefixed), `coop fork stop <name>`, and a running/idle
  column in `coop fork ls`. Declare a fleet in `.agent/fleet` as
  `<name> [agent] <tasks-path>` per line — `coop fleet split` writes that file for you.
- **`coop loop` takes a model.** `coop loop [claude|codex|gemini]` runs the main
  unattended loop with the agent you pick (default `claude`), instead of always claude;
  `COOP_LOOP_CMD` still overrides the iteration command outright.
- **Fewer silent defaults.** `coop fork merge` prints which branch it's landing onto (it
  rebases onto your *current* branch); a fork `--loop` that already has a queue says
  `--tasks` isn't being re-applied (use `--fresh` to reseed) instead of dropping it
  silently; `coop acp` names the model it defaulted to / which governor leads under
  fusion. `coop fleet ls` is gone — it was a pure alias for `coop fork ls`.
- **`coop fusion <agent>` picks the governor positionally** — `coop fusion claude`, to
  match `coop acp fusion claude`. The `--governor` flag is removed (use the positional
  form or `COOP_FUSION_GOVERNOR`).
- **Forks land by rebasing, and revalidate before they land.** `coop fork merge`
  rebases the fork onto your current branch (in the fork) and fast-forwards — linear
  history, no merge commits. Set `COOP_GATE` (e.g. `make check`) and it re-runs that
  gate in the box on the rebased tree, rolling back if it goes red — so "green" means
  green against your tree as it stands now, not the stale base the fork was cut from.
  `coop fork merge --all` lands the whole fleet as a revalidating rebase *queue*: each
  fork is rebased onto the result of the previous one and re-gated, stopping at the
  first conflict or failure and leaving the rest. **Commits get signed on land** if you
  sign (`commit.gpgsign=true`): the keyless box commits unsigned, then the rebase signs
  them with your host key (GPG or SSH) as it rewrites them — your signing key never
  enters the box.
- **Review and fleet management.** `coop fork review` now leads with a *brief* —
  commits, files changed, and the agent's own `.agent/LOG.md` reasoning — before the
  diff, so you get a map first. Review in your IDE instead of the pager with `--tool`
  (your configured `git difftool` — VS Code/JetBrains/Meld/vim), `--open` (open the
  fork in your editor — `$COOP_EDITOR`, then your `git config core.editor`, then a
  detected `code`/`cursor`/`zed`/`idea`), or override it entirely with `COOP_REVIEW_CMD`. `coop fork merge` runs a *policy check* that blocks
  secret-looking (`.env`, `*.pem`, `id_rsa`, …) or oversized files unless `--force`.
  Declare a fleet once in `.agent/fleet` (`<name> [agent] <tasks-path>` per line) and
  drive it with `coop fleet up | down` (list with `coop fork ls`); `coop fleet split <n>`
  round-robins `.agent/TASKS.md` into per-fork slices and writes a matching `.agent/fleet`.
- **Drive a fork (or the project) from Zed (ACP).** `coop fork <name> acp [agent]` fronts
  a fork as an ACP agent over stdio (pinned to the fork's path and the parent's image);
  `coop acp [agent]` does the same for the project in your cwd. Register them once in your
  *Zed user settings* (`agent_servers` is user-level — Zed rejects it in a project's
  `.zed/settings.json`); since `coop acp` resolves the repo from its cwd, one set of entries
  covers every coop project you open, forks included (`coop fork open <name>` opens a fork
  in your editor). Worktree Trust still applies; resuming a prior session rides on ACP
  `session/load`, which the editor drives.
- **Every box run gets your git environment.** The box has no ambient `~/.gitconfig`,
  so an agent would otherwise commit with no author ("Author identity unknown") and
  ignore none of your global ignores. coop now mounts a curated `~/.gitconfig` into
  every run — your global `user.name`/`user.email`, your global gitignore
  (`core.excludesfile`), and `commit.gpgsign=false` (the box holds no signing key, so a
  global `gpgsign=true` would otherwise fail every commit). Forks additionally carry
  the parent repo's *local* identity (which a global mount can't see) and its global
  gitignore into the fork itself.
- **`coop claude|codex|gemini` now pass your extra args through.** They run the agent's
  autonomous default command *with any args you add appended* (the autonomous flags are
  global, so it's safe even before subcommands), instead of dropping the defaults — so
  `coop claude --continue`, `coop codex resume --last`, `coop gemini -p "…"`, etc. keep
  coop's autonomy + MCP wiring. `coop fusion` forwards extra args the same way.
- **`-h`/`--help` works on subcommands.** `coop fork [verb] --help` prints the fork
  family help, and `coop loop|up|down|init|doctor|build|update|fleet --help`
  print the main help — all without needing a container runtime, instead of erroring
  (`unknown flag "--help"`) or running the command. (Agent commands still forward
  `--help` to the agent, so `coop claude --help` shows Claude's help.) Every short flag
  has a long form too (`-c`/`--continue`, `-d`/`--detach`, `-f`/`--force`/`--follow`),
  now shown in `coop fork --help`.
- **Host-side git in a fork can't be hijacked by the agent.** A fork is agent-writable
  down to its `.git/`, yet `coop fork merge`/`review`/`ls`/`rm` run host git in it. Those
  calls now disable hooks (`core.hooksPath=/dev/null`) and blank every config knob that
  shells out (`core.fsmonitor`, a forced `commit.gpgsign` + `gpg.program`, …), so a
  planted `.git/hooks/*` or malicious `.git/config` can't execute on your host. Signing on
  land reads your *parent* repo's signing config, never the fork's.
- **`.coopignore` hides repo-specific secrets.** Secret shadowing matched a built-in
  denylist by basename, so a committed `config/credentials.yaml` stayed visible to the
  agent. Add a repo-root `.coopignore` — basename patterns (any depth) or repo-relative
  path patterns — to shadow anything else. The defaults also grew (`*.keystore`, `*.p8`,
  `*.ppk`, `*.kdbx`, `*.ovpn`, `id_dsa*`, `.htpasswd`, `.dockercfg`, `.pgpass`); prove your
  setup with `coop doctor`.
- **`coop fork merge` won't land non-interactively without `--yes`.** `confirm()` returned
  its default with no TTY, so in CI or a pipe a merge would land work and delete the fork
  unprompted. It now refuses unless you pass `--yes` (which also skips the prompts
  interactively); `--all` is covered too.
- **`install.sh` verifies the release *signature*, not just the checksum.** When `cosign`
  is on `PATH` it verifies the Sigstore signature on `checksums.txt` (via
  `checksums.txt.bundle`) and fails closed before trusting it — so swapping both the
  archive and its checksum file is caught. Without cosign it keeps the SHA-256 check and
  says the signature wasn't verified; the README documents manual verification.
- **`coop build` is reproducible; `coop update` stays fresh.** The base image `FROM` is
  pinned to a specific `node:24` digest, so a `coop build` gets the same OS/runtime every
  time; `coop update` floats it back to the tag and rebuilds `--pull --no-cache` for the
  latest. Pin the agent CLI / ACP npm versions too with `COOP_AGENT_PACKAGES`.
- **The box ships `ripgrep`, `fd`, `jq`, and `tree`.** The search/inspect tools agents
  reach for constantly are baked into the base image (`fd` is symlinked from Debian's
  `fdfind`). Run `coop update` to pick them up.
- **The shared cache volume is writable by the agent.** `coop-cache` mounts at
  `/home/node/.cache`, but the path wasn't pre-created in the image, so a fresh volume came
  up root-owned and Go/npm/pip builds hit `permission denied`. The base and scaffolded
  images now pre-create it `node`-owned. Repair an existing volume with
  `docker volume rm coop-cache`, then rebuild.
- **The loop waits out Claude's *weekly* limit too.** `coop loop` already parsed
  `usage limit reached|<epoch>` and `retry-after` delays; it now also recognizes the
  current notice — `You've hit your weekly limit · resets Jun 18, 8pm (UTC)` — parses that
  reset and sleeps until it (a multi-day wait if need be), instead of mistaking it for a
  plain failure and stopping after a few retries.
- **`coop loop` detects an empty queue correctly.** Its todo scan matched any `[ ]`
  substring, so the legend line in `.agent/TASKS.md` always counted as work — the loop
  could never reach "queue empty" and the Stop-hook saw a phantom item on a finished queue.
  It now counts only real `- [ ]` task lines.
- **`coop login` re-opens the sign-in flow when you're already logged in.** It runs
  `claude auth login` (was a bare `claude`, a no-op once authenticated), so you can
  re-authenticate or switch accounts — e.g. off a rate-limited one. `coop <agent> login`
  works as well as `coop login <agent>`.
- **Command settings honor shell quoting.** `COOP_GATE`, `COOP_LOOP_CMD`, `COOP_RUN_ARGS`,
  and the `COOP_<AGENT>_CMD` overrides split with shell quoting (quotes group, `\` escapes)
  — without running a shell — so `COOP_GATE='bash -lc "make check && make lint"'` is three
  args, not five.
- **`coop init` scaffolding refinements.** Workflow skills now live once in
  `.agent/skills/`, with `.claude`/`.codex`/`.gemini` each symlinking to it (replacing
  three drifting copies and an orphaned root `skills/`). The scaffolded `.agent/` files
  model their own shape with an `## Example`, `TASKS.md` starts with an empty queue, and
  the `AGENTS.md` contract gains rules: tasks must be self-contained (workable from the
  BOOT files alone), don't create git branches unless asked, and `IDEAS.md`/`BACKLOG.md`
  hold a dump of your current thinking (spec included), not triage notes.

## 2.3.1

- **`--consult` makes the second opinion opt-in.** The peer-consultation directive
  introduced in 2.3.0 was always on; it now requires the `--consult` flag —
  `coop claude --consult` (or `codex`/`gemini`; in Zed, `coop acp claude --consult`).
  Off by default, otherwise unchanged: still injected only into the launched agent,
  still naming only the authenticated peers.

## 2.3.0

- **Agents can ask each other for a second opinion.** A normal `coop claude` (or
  `codex` / `gemini`) run now carries a light, optional directive: on a genuinely
  hard or risky call the agent may consult its peers **read-only and in parallel**
  to catch blind spots, then decide. It's injected only into the agent you launched
  (so peers it spawns don't recurse) and **names only peers that are authenticated**
  — if no other agent is logged in, nothing is added. The everyday, low-cost cousin
  of `coop fusion`, which mandates a full council + synthesis. Also covers
  `coop acp <agent>`; autonomous runs (`loop`, `dispatch`) are unaffected.

## 2.2.2

- **CI/CD supply-chain hardening.** Actions pinned to commit SHAs; CI runs with an
  explicit read-only token and no Actions cache (closes the cache-poisoning surface);
  `staticcheck` and the GoReleaser binary are version-pinned instead of tracking
  `@latest` / `~> v2`; checkout no longer persists the token in `.git/config`; release
  write scope is narrowed to the one job that needs it; and a Dependabot config keeps
  the pinned actions patched.
- `install.sh` now verifies the downloaded tarball against the release's
  `checksums.txt` and fails closed on a mismatch.
- **Signed, attested releases.** `checksums.txt` is signed keyless with
  Sigstore/cosign (a `checksums.txt.bundle`), and every archive carries a build
  provenance attestation — verify with
  `gh attestation verify coop_*.tar.gz --repo AndrewDryga/coop`. The repo also
  restricts Actions to an allowlist (GitHub-owned + goreleaser + cosign-installer)
  and requires approval for all outside-collaborator PR runs.

## 2.2.1

- **Bare `coop` now prints help instead of launching Claude.** Running an agent is
  explicit — `coop claude` (or `codex` / `gemini`) — so a stray `coop` never turns
  an autonomous agent loose on the current repo. `coop help` / `-h` are unchanged.
- **Per-language stacks dropped — `.tool-versions` is the single way to declare a
  toolchain.** `coop init --stack elixir|go|node|python` is gone; `coop init`
  auto-detects a `.tool-versions` and scaffolds the asdf `Dockerfile.agent` from it
  (`--stack asdf` forces it; a removed stack name now errors with a pointer to
  `.tool-versions`). The asdf image is a superset of the old per-language ones — it
  carries the build toolchain, `postgresql-client`, `procps`, and `inotify-tools`,
  and seeds `hex`/`rebar` when Elixir is present.
- The shared base box gains `postgresql-client`, `procps`, and `inotify-tools`, so
  the zero-config runtime path (bare `coop` on a repo with just a `.tool-versions`)
  matches a baked image. Run `coop update` to pull it.

## 2.2.0

- **`.tool-versions` honored by default — no `Dockerfile.agent` needed.** The base
  `coop-box` ships asdf and provisions a repo's `.tool-versions` toolchain at
  runtime (resolved from the cwd up the tree, or `~/.tool-versions`), cached in a
  shared `coop-asdf` volume so it installs once and is reused across repos. The
  first install of a toolchain can be slow (e.g. Erlang compiles), then it's
  instant. For a baked, reproducible image instead, `coop init` (or
  `--stack asdf`) scaffolds an asdf `Dockerfile.agent` that installs the same
  `.tool-versions` at build time. (`COOP_NO_ASDF=1` in agents/env opts out.)
- **`coop update`** rebuilds the box image fresh (`--pull --no-cache`) so the base
  image and the npm-installed agent CLIs + ACP adapters refresh to their latest
  (plain `coop build` is cache-bound and won't), then prints the resulting
  versions. The ACP adapters ship features often, so this is the easy way to stay
  current.

## 2.1.1

- Scaffolded stack images (`coop init --stack`) bake in the ACP adapters
  (`@agentclientprotocol/claude-agent-acp`, `@zed-industries/codex-acp`), so
  `coop acp` works in a project that has its own `Dockerfile.agent`. Without them
  it failed with `codex-acp: executable file not found`. An existing or
  hand-written `Dockerfile.agent` still needs the adapters added to its
  `npm install -g` line, followed by `coop build`.

## 2.1.0

- **Fusion mode — a governed council.** `coop fusion` runs one model as the
  *governor* (default `codex`; `--governor claude|gemini` or
  `COOP_FUSION_GOVERNOR`) and has it consult the other two **read-only and in
  parallel**, then synthesize the best result. It's a normal agent mode —
  interactive, headless, or in Zed via `coop acp fusion <governor>` (one
  `agent_servers` entry per governor to switch who leads). No extra service or
  MCP: the governor runs its peers (`claude -p --permission-mode plan`,
  `gemini --approval-mode plan -p`, `codex exec -s read-only`) from its own
  shell, and the fusion instruction is scoped to the governor only, so the peers
  it spawns never recurse.
- Smoother agent auth & first-run in the box. `coop login codex` uses the
  device-code flow (`codex login --device-auth`) — the container has no browser
  and codex's localhost OAuth redirect can't reach the host. Codex's "Do you
  trust this directory?" prompt is pre-answered (`[projects."<dir>"] trust_level =
  "trusted"`) and so is Gemini's folder-trust (`security.folderTrust.enabled =
  false`), matching Claude's first-run seeding — the box is the sandbox. All
  merged/idempotent, so an explicit choice and your other settings are kept.
  Interactive runs also propagate your terminal's `TERM`, so the agents' TUIs
  render in full color instead of warning about a basic terminal.
- Gemini no longer fails to launch on an empty `settings.json` ("Unexpected end
  of JSON input"); the box seeds valid JSON when that file is missing or blank
  (your own settings are preserved).

## 2.0.0

- **Renamed to Coop.** The binary is now `coop`, the image is `coop-box`, config
  lives in `~/.config/coop`, and env vars use the `COOP_` prefix (previously
  `agent-box` / `agent` / `AGENT_`). `install.sh` migrates an existing
  `~/.config/agent-box/agents` over on upgrade.
- The loop rides out rate limits. When the model hits a rate or usage limit
  mid-run, `coop loop` no longer spins on a fixed retry — it parses the reset
  time from the agent's own output (Claude's `usage limit reached|<epoch>`, or a
  `retry-after` delay), waits until then with a countdown, and resumes the same
  item, so an unattended overnight run survives the daily cap instead of burning
  retries against it. Non-limit failures back off and stop after a few in a row.
- Unified working directory and history across modes. `coop`, `coop loop`, and
  `coop acp` now all mount the repo at its real host path (not `/workspace`), so
  each agent's per-project session history is shared — a thread you started with
  `coop loop` is there to resume when you open the repo in Zed. `COOP_WORKDIR`
  still overrides the mount path for the old `/workspace` behavior.
- One-line install + releases: `curl -fsSL .../install.sh | sh` downloads the
  prebuilt binary (no Go, no clone). GoReleaser publishes cross-platform binaries
  — with auto-generated, categorized release notes — to GitHub Releases on every
  `v*` tag. CI runs gofmt, vet, staticcheck,
  tests, build, and shellcheck.
- Updated to current toolchain + base images: Go 1.26; GitHub Actions checkout
  v6, setup-go v6, goreleaser-action v7; the box base on node:24 (LTS); and the
  scaffolded stacks on python 3.14 / golang 1.26 / elixir 1.20-otp-29 /
  postgres 18 / redis 8.
- Rewritten in Go: `coop` is now a single static binary (no bash, no runtime
  dependencies) built with `go build`. Same commands, same box, same
  secret-shadowing — faithfully ported, with the security core (mount
  computation and run-arg assembly) now pure and unit-tested, and proven
  end-to-end by `coop doctor`.
- Native MCP generation: Gemini's `settings.json` and Codex's `config.toml` are
  produced in Go, so the host no longer needs `python3` for any agent's MCP.
- `coop init` templates and the workflow skills are embedded in the binary, so
  scaffolding needs no repo checkout (a step toward a fork-free install).
- Config moved to `~/.config/coop/` (XDG): `agents/`, `mcp.json`,
  `INSTRUCTIONS.md`, `env`, and `coop.conf` live there, decoupled from any repo.
  `install.sh` seeds it from an existing in-repo `agents/` on upgrade. The conf
  file is parsed as `KEY=VALUE` lines (the environment still wins over it).
- `coop help` and `coop version` no longer require a container runtime.
- Claude login now persists. The box sets `CLAUDE_CONFIG_DIR` to the mounted
  `~/.claude`, so Claude's account/onboarding state — which it keeps in
  `~/.claude.json` in `$HOME` — survives the disposable container instead of being
  lost every run (a latent bug from the bash version too; credentials already
  persisted, but without the config file Claude re-showed its login screen).
- Terminal detection uses a real isatty ioctl rather than a character-device
  check, so `coop run … < /dev/null` (and other char-device stdin) no longer
  wrongly requests a docker tty.
- Fresh boxes skip Claude's first-run prompts. Each run pre-answers the theme
  picker, the folder-trust dialog, and the bypass-permissions warning (the box is
  the sandbox) by seeding `settings.json` and `.claude.json` in the mounted config
  — merged, so the login and any settings you've chosen are preserved. A fresh
  install goes straight from one login to working.

## 1.6.0

- One file, every agent's MCP: define servers once in `agents/mcp.json` (the
  standard `{"mcpServers": {...}}` shape) and `coop` wires them into all three
  on launch — Claude via `--mcp-config`, Gemini merged into its `settings.json`,
  Codex converted to `[mcp_servers.*]` in its `config.toml`. The Gemini/Codex
  configs are generated read-only on top of your existing files (never modifying
  them; `mcp.json` wins on a name clash), which needs `python3` on the host;
  Claude needs nothing. `cp agents/mcp.json.example agents/mcp.json` to start.

## 1.5.0

- `coop acp [claude|codex|gemini]` — run the box as an ACP agent over stdio, so
  editors like Zed can drive the sandboxed agent. Mounts the repo at its real
  host path (so the editor's absolute paths resolve), attaches stdin without a
  pty, and keeps secrets shadowed. Point Zed's `agent_servers` at
  `command: "agent", args: ["acp", "claude"]`.
- The default image now bakes in the ACP adapters (`@zed-industries/claude-code-acp`,
  `@zed-industries/codex-acp`; Gemini's is built in) and trusts any git worktree
  (`safe.directory '*'`) so git works on the host-path mount.

## 1.4.0

- Ship generic workflow skills — `/plan`, `/work`, `/batch`, `/verify-api` — under
  `skills/`. `coop init` installs them into the repo's shared `.claude/skills/`
  (Codex gets them via the symlink), without clobbering ones you've edited, and
  the generated `AGENTS.md` points the agent at them. Adapted from the production
  set (emisar), with the repo-specific parts (Iron Laws, Elixir contexts) removed.

## 1.3.0

- `coop init` now scaffolds the full `.agent/` working set — `TASKS.md`,
  `BACKLOG.md`, `LOG.md`, `PENDING_DECISIONS.md`, `IDEAS.md`, `rules/` — and the
  generated `AGENTS.md` documents each one's role, so the canonical manual that's
  re-injected after a compaction tells the agent how to use them. Matches the
  layout used in production (emisar). Everything but `rules/` stays git-ignored.

## 1.2.0

- `coop dispatch <name>` — the fleet unit: clone into an isolated workspace,
  seed it with that agent's queue slice (`.agent/TASKS.<name>.md`), run the loop.
- `coop init` now wires the tool-neutral setup: `CLAUDE.md` and `GEMINI.md`
  symlink to the canonical `AGENTS.md`, and `.codex/skills` shares
  `.claude/skills`. Real (non-symlink) instruction files are never clobbered.
- Explicit `COOP_IMAGE` now overrides image selection (lets a dispatched clone
  reuse its origin's image); `COOP_SERVICES_NET` lets a fleet share one db/redis.
- Source-guarded entrypoint (`main`) so the script is unit-testable; added
  `test/unit.sh`, a `Makefile`, and CI (shellcheck + unit tests).

## 1.1.0

- Three agents in one box: `claude`, `codex`, `gemini`, each with autonomous
  defaults; `coop login <agent>`.
- Per-agent auth + settings in `agents/<agent>/`, mounted into the box; `agents/env`
  for API keys; one `agents/INSTRUCTIONS.md` wired into all three agents' native
  instruction paths, with per-agent overrides taking precedence.
- Per-project environments: `coop init --stack <elixir|python|go|node>` writes a
  `Dockerfile.agent` (toolchain) and `compose.agent.yml` (services); `coop up`/
  `down` run sibling Postgres/Redis the box reaches by name; per-project image tags.

## 1.0.0

- The box: `coop` runs a sandboxed agent on the current repo, with repo secrets
  shadowed out of reach (tmpfs over secret dirs, read-only decoys over secret files).
- `coop doctor` proves isolation by attacking it; `coop clone` hands off a
  secrets-free workspace with no reachable remote.
- The autonomous loop: `coop loop` works `.agent/TASKS.md` with disposable
  sessions and an audit pass; `coop init` scaffolds the queue + Stop/commit hooks.
