# Changelog

## Unreleased

<!-- Add entries here as you ship; this heading is renamed to the version on the next release. -->

- **`coop usage` removes cached records of deleted transcripts.** The usage cache kept a record for
  every transcript it ever read, so it grew as Claude deleted month-old transcripts and as accounts
  and history folders were removed. Now `coop usage` without a provider, once it has read all
  history, deletes the records it no longer needs. A one-provider check or an interrupted check
  deletes nothing, and neither does a history folder Coop could only partly read. Coop removes only
  its own record files.

- **Signing keeps the IDs of signed and pushed commits.** Coop signs the commits agents make in a
  box after ordinary agent runs, after each loop cycle, at the end of a loop run, and with
  `coop sign`. Each pass used to rewrite every commit in its range, signed ones too, so a commit
  another agent made in the same checkout, or one you had already pushed, got a new ID, and a
  pushed branch then needed a force push. Signing now starts at the first unsigned commit, and the
  signed commits before it keep their IDs. It never rewrites a commit the branch's upstream already
  has: when the first unsigned commit is already pushed, Coop stops and says so, and `coop sign`
  then signs only the commits you have not pushed. When every commit is already signed, `coop sign`
  leaves them alone and says so.

- **The daily cleanup of what killed runs leave behind now finishes.** Coop marked the day's
  cleanup done before starting it, so a quick first command such as `coop tasks ls` cut it short
  and everything waited another day. A box that went away while the cleanup checked it also
  cancelled the whole pass. The cleanup now starts alongside commands that run boxes, such as
  `coop claude`, `coop acp` and `coop loop`, counts only once it has finished, and skips a box
  that is already gone. A pass that could not ask Docker which files boxes use does not count, so
  the next box tries again. It also removes the Docker and Compose client folders killed runs
  leave behind, the Docker ones only from a temp folder no other user can write to. It still
  removes only Coop's own entries that are over an hour old and that no box uses.

- **`coop tasks` points out bulk left in the task queue.** Agents sometimes kept compiled
  programs, release tarballs and toolchains in task folders, and left release files next to the
  state folders, where they stayed until someone deleted them. `coop tasks done` now warns when
  the folder it archives holds a compiled program or more than 25 MB, and `coop tasks` warns about
  any file or folder in `.agent/tasks` besides its README, state folders and backlog. Hidden files
  are left alone, and neither warning deletes anything or stops the command. A new project's tasks
  README says what to keep instead: the commit, sha256 and build command.

- **Box images check asdf before installing it.** Coop's box images download asdf, the tool that
  installs the versions in a project's `.tool-versions`. A build now compares that download with the
  checksum Coop pins for its architecture and stops if they differ, so a changed or tampered
  download never reaches a box. This changes the filtered client image: the next filtered run sets
  up networking again (an editor session asks you to run `coop net setup` first), and a filtered
  project with its own `.agent/Dockerfile` needs `coop build --egress filtered` once.

- **Coop no longer runs your `sqlite3` on Codex's log database.** Before launching Codex, Coop used
  whatever `sqlite3` was on your PATH to add a trigger that stopped Codex writing its feedback log.
  Since 5.0 that database lives inside each box and is removed with it, and the Codex that Coop
  ships trims the log itself, so the trigger only touched an old copy in your credential home.

- **Preset roles move on when a provider reports a rate limit with single quotes.** Inside the box,
  `coop-consult` and `coop-delegate` missed a limit written as `{'status': 429}` or `{'code': 429}`,
  so a role could retry or stop on the limited target instead of moving to its next one. The in-box
  check now uses the same patterns as Coop's own rate-limit detection.

- **`coop approve` no longer labels each rule.** The rule diff no longer repeats "already approved",
  "new request" or "no longer requested" after every rule. The `+` and `-` markers say it, and the
  `NETWORK RULES` heading explains the ones it uses, so the meaning survives without color.
  `coop net` keeps its labels: there they tell a requested rule from an approved one.

- **A running editor session picks up the network rules you approve.** Before, `coop approve`
  changed only new runs, so a filtered `coop acp` session kept the old rules until the editor
  reconnected. Now the session picks up the change once no prompt is running: it starts a new box
  under the newly approved rules and only then stops the old one, so the conversation continues,
  and the editor shows one line saying so. A running box's own rules never change. If the approval
  can't be applied, for example because the project file changed again, or the new box stops before
  the switch, the session keeps the rules it had and says why. After `coop net forget`, a running
  session keeps its rules until you reconnect the editor. `coop approve` now asks to approve access
  "for new runs and running editor sessions".

## 11.3.0

- **Usage includes recent requests in large histories.** Old transcripts no longer fill a scan
  budget that excludes newer usage. Coop caches parsed token records from unchanged files,
  rereads changed files and recovers later requests after an oversized JSONL record. The cache
  contains no conversation text; estimates still use the rolling 30-day window and current rates.
  GPT-6 Sol, GPT-5.6 Terra and GPT-5.6 Luna now have their documented API tariffs.

- **A worker removes the repository copies no session can still use.** A worker staged a copy of
  every repository a job named and never removed one, so a controller that started sessions
  often filled the worker's disk: on one install, about 3 GB an hour. Each maintenance pass now
  removes copies no open or closed session uses. It keeps each repository's newest copy and its
  first download, a task session's own copy for 30 days, and any copy used in the last hour.

- **A slow repository fetch no longer holds up the worker.** A worker runs one command at a time,
  and a session create waited for every repository it had to fetch: on one install a create took
  18 minutes, and every other session's work waited behind it. Every new copy a create needs now
  starts fetching in the background at once, its grant included, and the create waits for them
  three seconds in all. It is then tried again until its repositories are ready, every 15 seconds
  at most, while the worker runs every other command. A repository with submodules does too. Only a
  repository's first copy asks for its grant first, and its download then runs in the background.
  Finished downloads release their bookkeeping before the worker reuses the copy.

- **A branch that moved on no longer refuses its job.** A worker staged a job's repository only
  if the branch still pointed at the job's commit, so a create failed whenever the branch moved
  while it was being set up, and a task's next session failed for good once its main branch had
  moved. The commit now only has to be on the branch: its head or in its history. A branch
  rewritten past it, by a force push, is still refused.

## 11.2.0

- **Task tools shut down cleanly.** A helper's normal exit no longer loses its final output
  or reports a false shutdown error.

- **Invalid native account access stays stopped.** Another request cannot slip through
  while the first account refusal is being cleaned up.

- **Filtered runs start with separately isolated helper clocks.** Coop now verifies the
  helpers' actual clock offsets instead of requiring matching namespace labels. Network
  restrictions, credential protection and expiry checks stay in place.

- **Usage includes native CLI history outside Coop.** Supported Claude, Codex, Gemini and Grok
  histories on this computer now contribute to recorded estimates, with copied requests counted
  once and unmatched accounts kept unassigned. Optional host-only provider reports show API
  organization/team spend, or selected Google billing SKUs, separately from local estimates.
  Reporting dates, scope, partial coverage and failures remain visible; these are not personal
  subscription bills.

- **Usage makes capacity and estimates easier to read.** Claude and Codex show reset counts and
  reported expiry without redeeming them. Credit balances use readable digit grouping, reset
  times are quieter and dot-free, and account estimates show their `/30d` period. Provider
  subtotals and a highlighted total sum recorded API-value estimates, with incomplete coverage
  marked partial; these are not credit balances or bills.

- **A turn that goes quiet after its tools stops.** A model that sends nothing for 15 minutes
  after its tools answered used to hold its turn until the turn deadline, while its provider kept
  pulsing. The turn now fails with `acp_quiet`, which a caller can retry.

- **An early exit says what kind of exit it was.** When a model process closed before its
  response, the turn only said "ACP child closed before its response". It now also says how the
  process ended and what its stderr looked like: line and byte counts, a digest that groups
  identical exits, and word classes such as rate limit or authentication. It never includes the
  stderr text.

- **Filtered networking works where 1.1.1.1 is intercepted.** The network gateway's resolver
  now dials both of Cloudflare's addresses, 1.1.1.1 and 1.0.0.1, at once and uses the first that
  proves its certificate, so an address that is blocked, hangs, or answers with another
  certificate no longer stops name resolution. Both must present cloudflare-dns.com; nothing falls
  back to an unverified resolver.

- **A quiet turn's pulse says what the provider is still sending.** `provider.alive` now carries
  `last_kind`, the latest frame's method or session update type. It never carries frame content.

- **A reservation released mid-scan no longer fails a session create.** When another session
  released its workspace reservation while Coop was reading the registry, the missing file was
  reported as a registry problem and the unrelated create failed with "no such file or
  directory". A released reservation now counts as absent; an entry that is still there but
  unreadable is still refused.

- **Two project reviews starting together no longer fail on their shared lock.** Creating the
  review lock could fail with "no such file or directory" when another review created it at the
  same moment. Coop now tries that open again.

- **Known issue in filtered Claude sessions.** An intermittent proxy disconnect remains
  under investigation. The networking fixes above do not claim to repair that earlier failure.

## 11.1.0

- **Updates prepare the image your project actually uses.** After installing a new release,
  Coop uses that version's image definitions and the project's effective network mode.
  Filtered projects no longer finish updating with only an unused ordinary image built.
  When upgrading from v11.0.2 or earlier, run `coop update --box-only` once using the newly
  installed binary; the older updater cannot use the new preparation flow.

- **Build and continue from an interactive launch.** When a filtered project needs a build,
  Coop offers the next step before slow launch setup. Review the saved build snapshot and
  approve it once; unattended runs still require an explicit host build. Later edits are not
  approved, and project builds remain on the selected Docker daemon.

- **Image checks distinguish missing builds from Docker failures.** Missing or changed project
  builds are explained early. Docker observation failures retain their real cause, and
  `update --check` no longer reports ordinary-image records for filtered projects.

- **Warm sessions keep their full idle interval.** Slow startup or a long model turn no longer
  consumes the session's next idle interval. Broker credentials still renew continuously and
  expire closed if renewal stops.

- **Native proxy disconnections are easier to diagnose.** Helpers record a safe failure
  category when protected traffic stops, without logging account details or credentials.

- **Reviewed commits say what they change.** A review request may name the commit message
  (`commit_message`, up to 4,096 bytes); the published commit carries it instead of "Reviewed
  changes", still followed by the `Coop-Review` trailer. Workers advertise
  `review-commit-message:1` once their daemon accepts the field.

- **MCP credential brokers start under strict file permissions.** The broker's configuration
  and secret files are written with their modes set, so a private umask no longer leaves them
  unreadable to the broker's own user.

- **A parked session is not refused while Coop checks it.** Restore, checkpoint, task binding
  and review wait up to ten seconds for a session runtime that Coop's own cleanup briefly holds,
  instead of refusing as if a turn were running. A session with a turn running or queued is
  still refused at once.

## 11.0.2

- **Providers start under strict file permissions.** Native provider helpers now receive
  readable public configuration when the host uses a private umask. Secret custody remains
  owner-only and outside the provider box.

- **Provider sessions recover from brief credential-file read failures.** Transient
  host-to-VM reads can recover within the existing load deadline, instead of stopping after
  three reads. Invalid accounts still fail closed.

- **Task bookkeeping stays out of code changes.** Verified controller-managed task files
  no longer appear as unfinished product changes, including after checkpoint recovery.
  Actual code changes and unrelated files remain visible.

- **Credential completion fills in accounts and actions.** Shell completion now fills in
  account names and actions for `coop credentials`.

## 11.0.1

- **Codex starts with the selected workspace.** Native account discovery now matches the
  public broker identity while preserving upstream workspace routing and sibling accounts.

- **Claude loads managed settings and organization policy normally.** The broker admits exact
  native policy requests for subscription and API-key accounts, eliminating false access-denied
  warnings without changing genuine upstream policy or authentication errors.

- **Provider sessions survive transient VM file reads.** Credential leases reopen briefly
  incomplete or missing atomic publications instead of permanently disconnecting a healthy
  session. Revoked, expired and invalid accounts still stop protected traffic.

- **Slow activity reads no longer make worker polls look stale.** Workers refresh the outgoing
  timestamp before sending, so delayed local reads do not cause clock-skew refusals.

- **Fleet reads stay responsive during slow commands.** Workers can inspect sessions and
  operations while another command is still running, with bounded concurrency and ordered writes.

- **Stopping services cleans up filtered networks.** `coop down` now removes unused
  project networks left by filtered runs, while keeping attached networks and stored data.

- **Same-provider peers work independently.** Naming the lead's provider with `--peer` now
  mounts and authorizes its consult wrapper while preserving the lead's model and effort,
  including in editor sessions. The peer shares the lead's selected account.

- **Status checks no longer interrupt fork startup.** Concurrent Git refreshes now keep their
  temporary files separate. Review setup also reports the real Git error instead of branch loss.

- **Background fork stop finishes cleanup.** Stopping a loop now retires its dead sandbox records
  as well as its containers, so stale cleanup state does not block later work. Paused tasks remain.

- **Editor switches give useful retry guidance.** A request during an agent switch now asks you
  to wait and retry instead of incorrectly suggesting that the thread is unavailable.

- **Editor sessions load shared MCP tools.** The editor connection now receives the same
  validated tool configuration as other Coop runs, including after a restart. Conflicting
  editor and shared tool names produce a setup error instead of silently replacing a server.
  Servers with no arguments, environment variables or headers are no longer silently dropped.

- **Unsupported Haiku effort is caught before startup.** Haiku 4.5 has no reasoning-effort
  control; use `claude:haiku` without a suffix. Coop now explains this instead of silently
  dropping the requested level in the CLI or failing the editor's first message.

- **Model discovery respects project networking.** Claude and Gemini catalog refreshes now use
  normal host admission for the selected account, including filtered projects. Failed refreshes
  retain the last good list and explain setup failures without exposing provider diagnostics.

- **Grok models use your Coop account.** Catalog discovery no longer requires a separate host
  Grok login or host CLI; it asks the boxed client using the same selected account as a Grok run.

- **Existing Grok sign-ins migrate safely.** Coop accepts normal native writer-lock permissions
  while preserving the lock, excluding active writers and keeping credential storage private.

- **Credential startup checks scale to busy Docker hosts.** Complete mount inventory uses
  bounded batches while preserving daemon identity, read-only mounts and stopped-container
  safety checks; unavailable or incomplete observations still refuse startup.

- **Retired test containers no longer block unrelated sign-ins.** Removed temporary bind paths
  on stopped containers are compared safely; credential overlaps, unresolved live mounts and
  ambiguous symlinks still prevent migration.

- **Native accounts start in filtered projects.** Generated native-home and public broker-helper
  mounts now pass the same custody checks as other binds, preserving quoted paths and refusing
  missing sources or unsafe overlaps.

- **Model discovery allows normal box startup.** Native account setup no longer consumes the
  catalog request deadline. Both startup and catalog requests remain bounded and cancellable.

- **Grok catalog refresh initializes its broker.** Discovery uses the native external helper
  before requesting models, without device sign-in or exposing account tokens to the box.

## 11.0.0

- **Repository-local native history and host-only provider credentials.** Each project keeps its
  own complete provider home, including native resume, settings and history indexes. Older
  transcripts import only when their repository ownership is known; originals remain intact and
  deleted sessions are not resurrected. Editor homes survive account switches. A run-local broker
  serves API-key and subscription accounts while the host owns renewal; real provider grants no
  longer enter coding boxes. Online provider runs require Docker. See `MIGRATING.md` before reusing
  existing accounts, and manage sign-in/removal through the host.

- **Patched Go networking.** Builds and box helpers use Go 1.27.2 and the updated networking
  library, addressing the October Go security advisories.

- **Optional isolated-write forks.** `coop fork <name> <target> --isolated` gives native Git its
  own metadata/objects and fences parent checkout/Git stores from box and service mounts.
  Review uses exact private committed custody; checks run on a separate clone. Publication
  rejects unsafe intermediate history without a force bypass and retains a durable candidate
  and forward-reconciliation journal after interruption. Shared-write defaults stay unchanged.

- **Filtered networking works with Docker 27 workers.** Setup could pass its network checks but
  fail to stop its helpers because Docker 27 calls the stop option `--time`, not `--timeout`.
  Cleanup now uses the portable `-t` flag and still checks that the exact workload stopped.
  Filtered repository-read-only worker jobs also keep their separate output mount writable without
  an unsupported mount spelling. Worker docs explain CLI, Buildx and shared-path requirements.

- **Filtered launches work after `coop up`.** A filtered box launched after `coop up` failed with
  "container … is not connected to the network …_filtered": Compose rebuilt the filtered services
  network and could not detach a service that `coop up` had moved off it. The launch now declares
  that network as it already is, so Compose only recreates the containers, and services keep their
  data, anonymous volumes included. A failed Compose step now ends with Compose's own reason.

- **A second landing page: put your agents to work while you sleep.** `coop.dryga.com/orchestrator.html`
  is for teams that want agents working through many tasks unattended; the homepage stays the
  sandbox story. A pinned `coop loop` board lives through one night, 22:00 to 07:00, beside seven
  steps: tasks queue up, each gets a fresh agent, is checked and committed, accounts rotate at
  their limits, a question waits while the work goes on, a final review sends one task back, and
  morning shows 120 done and one decision. Every page's menu links it as Orchestrator, and the
  homepage's Task queue leads to it. Its figures are its own, each checked in its source.

- **Final review counts only its own pending reviews.** `coop loop` refuses to load more than
  4,096 pending final-review records, and it counted every repository's records on the machine,
  not just the queues it was loading, so enough of them elsewhere would have stopped every final
  review. It now counts only the selected queues' records.

- **Boxes start faster in a cluttered checkout.** Before every start, co:op checks each path in
  the checkout, gitignored task folders included, against 64 secret-file patterns, so it can hide
  the matches. With 40,000 paths that took about half a second. Two string comparisons now rule out
  almost every name before the full pattern match, with the same answers, so the check takes about
  70 ms. In co:op's own checkout, a filtered start went from 3.18 s to 2.62 s, against 2.30 s for
  a clean clone.

- **Filtered boxes start faster.** A filtered launch checks about 74 times that Docker is still
  the daemon it bound to. Each check started a `docker info` process (about 40 ms on a busy Mac);
  co:op now sends that one request over the same socket itself (about 4 ms), and still checks
  every time. On a busy Mac (load 9 to 15), the median filtered start went from 3.75 s to 2.89 s.

- **`coop tasks watch` sits idle quietly.** The board re-read every task's records, done ones
  included, on each 400 ms tick: on a 650-task queue that kept a fifth of a CPU busy while nothing
  changed. It now reads again only when a task folder or a listed task's files change, and at
  least every 10 seconds; leases are still checked every tick. Measured on this repo: 21% CPU
  before, about 1% after.

- **Workers keep the checkout path to themselves.** What a worker tells its controller about a
  turn (tool titles, commands and their output, thoughts, plans) still named the session's checkout
  by its absolute path, though the docs said the worker removes it: the worker looked for the path
  in a field the session service never filled. The service now sends it with every narrated
  event, so the controller sees `lib/a.go` where it used to see `/Users/you/…/lib/a.go`.

- **Session workspace and checkpoint calls check their headers.** `POST /v1/sessions/{id}/workspace`
  and `/checkpoint` now answer `400` to a query string, a Content-Type other than
  `application/json` or a repeated `Idempotency-Key`, like every other session mutation. Before,
  such a request reached the service. Workers already send these headers.

- **Park a decision for later.** `coop tasks park <id> ["<reason>"]` answers a blocked task with
  "not now": it moves to the backlog with its decision still open, so `coop loop --preflight` never
  re-queues it. Press `:b` in `coop tasks decisions -i` to do the same. `coop backlog promote`
  brings a parked task back blocked, with its question.

- **Docs in plain words.** The README is now a short introduction with the install, a quickstart and
  a tour of each feature. The detail it used to carry moved to reference pages under `docs/`:
  the box, agents and accounts, forks, the loop, editors and MCP, evaluations and configuration.
  Contributor guidance moved to `CONTRIBUTING.md`, and the guide's troubleshooting table now has
  every row.

- **Current models in the starter preset.** `coop presets init` now writes Opus 5.5 at xhigh as
  the lead, falling back to GPT-6 Astra; Fable 5.1 at max effort as the thinker; GPT-6 Astra as the
  critic; and Gemini 3.8 Flash for fast edits. Help, docs and the `coop models` examples name
  current models too. Presets you already have keep their models.

- **Simpler usage output.** Keep progress bars and show limits before the 30-day API estimate,
  without default tags or repetitive footnotes. Bars align across accounts, shared editor usage
  has a clearer label, and each provider's columns fit its own labels. Percentages right-align so
  resets line up, balances and the dimmed 30-day `Σ≈$…` totals start where `100%` does, totals
  appear only when usage has API pricing, and API-key or Vertex sign-ins without limits fit on
  one line, such as `personal (API key)  Σ≈$2.62`. Resets show the time left and the moment in
  UTC, such as `resets in 2d 2h (Oct 4, 08:35 UTC)`. Select one account for history and pricing
  details.

- **Clearer installer output.** Installation, optional shell setup and sandbox checks use
  separate readable blocks, with final examples for starting an agent and finding help.

- **Clearer fork examples.** Fork help calls its example fork `myfork`, so
  `coop fork myfork claude` no longer reads like a sign-in command.

- **Fork editor sessions start without a target.** `coop fork myfork acp` starts the agent the
  fork was created with, so an editor entry needs only the fork's name; choose the model and
  effort in your editor. A target such as `claude:opus` still picks the agent.

- **Loop report grammar.** A task the final review reopened once reads "was reopened once by the
  review", not "was reopened 1 times".

- **Plainer claims.** The help header, README and docs describe the box as a sandbox that holds
  only your project, not one an agent "can't escape", and link what the agent can still reach.

- **A commented `.coopignore` entry is flagged.** A launch warns when an entry has a comment after
  its pattern (`prod.yml  # note`): only a line that starts with `#` is a comment, so such an entry
  is one pattern that hides nothing. The README example puts its comments on their own lines.

- **Claude's MCP logs stay with their run.** Every box shares one `~/.cache` volume across
  projects and accounts, and Claude Code kept per-project MCP logs (tool traffic included) in it,
  where any other box could read them. Each run now gets its own empty
  `~/.cache/claude-cli-nodejs`; the rest of the cache stays shared.

- **`coop init` warns on Apple container.** Filtered networking, which `coop init` turns on,
  needs Docker. On Apple `container`, init now says to start or install Docker; online agents
  require it even with open networking. The project file stays filtered, so
  teammates on Docker keep the filter.

- **Agent help lists the network options.** `coop help claude` (and codex, gemini, grok) now
  shows `--egress`, `--allow-domain` and `--egress-rules`, which every launch accepts, so you can
  find how to let one run reach a package registry.

- **Filtered boxes tell agents where toolchains come from.** The note an agent gets inside a
  filtered box no longer promises that `.tool-versions` is installed on start, which only open
  boxes do. It says a pinned toolchain comes from the project's `.agent/Dockerfile`, which
  `coop init --stack asdf` writes from `.tool-versions`.

## 10.1.2

Changes since v8.1.0 include provider-client updates, `coop usage`, and the new controller-worker
workflow. Follow the [upgrade guide](MIGRATING.md#v1012-upgrading-from-v810), including the
session-state backup, before upgrading.
[Compare the public upgrade](https://github.com/AndrewDryga/coop/compare/v8.1.0...v10.1.2).

- **Fix large worker result delivery.** Results containing Unicode separators stay within
  transmission limits on Go 1.27.

- **Refresh dependencies and client compatibility.** Update Go/tooling, CI actions, container
  bases, release tools and locked provider clients. Patched npm transitives remove the reported
  fast-uri and ip-address advisories. Codex ACP2 uses its current versioned phase metadata and
  terminal output stream. Gemini's effort and sign-in settings are image-owned system files,
  satisfying its ownership checks without shared settings mutation or host ownership changes.
  Codex workspace authentication refusals advance configured account fallback. Native Claude
  startup verifies the selected account/trust config across VM mounts before launching.

- **Retain typed terminal exit evidence for controllers.** Command output deltas and numeric
  exit codes/signals survive output truncation and pass through bounded controller activity.

- **Inspect limits and recorded token value with `coop usage`.** Read all configured providers
  or one `provider@credential`, preserving independent failures and local reset times. Native
  histories produce a deduplicated rolling 30-day estimate at current standard API token prices,
  including subscription activity. Missing prices, partial records and unknown shared-editor
  credential attribution stay explicit; lossy retained turn aggregates are never added to it.

- **Preserve multiline instructions when freezing eval suites.** Authored instructions retain
  exact leading newlines and indentation in reloadable frozen manifests.

- **Add trusted runtime profiles for agent evals.** Custom cases can bind clean external
  build inputs to an explicitly approved immutable image, `/app`, fixed CPU/memory/PID caps
  and separate candidate/verifier budgets. Original profile identity and content remain
  authoritative; retained copies cannot approve builds. Candidate networking stays provider-only
  filtered and grading stays offline. Reports disclose the declared but unenforced/unmeasured
  storage limit; these profiles do not reproduce official benchmark environments.

- **Deliver ACP cancellation before teardown.** A prompt interrupted just after delivery still
  receives a bounded cancellation attempt. Successful cancellation gets a short EOF-drain window
  before forced termination, retaining larger restricted/filtered cleanup allowances.

- **Stop admitting worker commands after shutdown.** Cancellation racing a successful controller
  poll no longer starts another source/body download or recreates an acknowledged command receipt.
  Successful response acknowledgements and in-flight cleanup retain their existing behavior.

- **Breaking: upgrade configuration and controller contracts together.** `coop.conf` rejects
  unknown, duplicate and retired keys; move retired loop settings into `.agent/loop.yaml`.
  Podman and `COOP_AGENT_PACKAGES` are no longer supported. `coop tasks clear`, `split`
  and `flags`, `coop fleet`, `coop fusion`, `coop worker`, `coop sessions serve` and `policies`
  are removed without aliases. Use presets/peers and direct detached fork loops for composition.
  Use `coop tasks rm --all-done`, the canonical shared queue and the connection command below.
  `coop approve` replaces `coop net approve`; `coop net blocked` replaces `net explain`.
  Back up session state before its schema-25 upgrade; older binaries cannot reopen the new root.
  See [MIGRATING.md](MIGRATING.md) for required actions and preserved data.

- **One outbound worker connection, generic controller-owned jobs.**
  `coop sessions connect --controller <https-url> --token-file <path>` owns enrollment and
  its private local service; optional `--state` and `--ca-file` select storage and trust.
  There is no worker JSON configuration, local execution-policy catalog or separate serve command.
  Protocol-v2 jobs freeze exact source, targets, execution/network settings, work/check environments,
  literal review argv and finite CPU, memory and PID caps. Controllers require live `job-setup:2`
  proof. Historical evidence remains readable; unsupported old execution authority is not invented.
  Provider accounts still need normal worker authentication: controller-supplied model credential
  exchange is not part of this release.

- **Structured completion and caller validation survive interruption.** Remote turns can bind
  a JSON Schema and its exact digest, repair invalid output in the same native session, and
  retain schema-valid candidates for explicit caller acceptance or rejection. Only accepted,
  digest-bound bytes publish; repair/review budgets are bounded. Pending candidates and durable
  decision receipts survive restart, while retryable runtime cleanup cannot silently decide them.

- **Complete source custody and controller publication.** Workers fetch pinned GitHub source
  with repository-scoped host grants, verified LFS objects and explicitly authorized recursive
  submodules; repository-controlled URLs cannot redirect those grants. Checkpoint transfer/restore,
  acknowledged activity and command receipts survive bounded retries and restarts. Controllers can
  request reviewed changes be published to a draft pull request; large bundles travel separately
  from control messages. Storage admission distinguishes protected, disposable and unknown usage;
  its watermarks are not per-workspace disk quotas.

- **Review uses the captured parent and frozen job setup.** Controller gates retain accepted
  command, environment, networking and resource limits, ignoring repository box policy and ambient
  worker launch settings. Prepared candidates now name their trusted parent for `COOP_REVIEW_BASE`,
  so gated reviews can start. Stale or mutated evidence cannot become publishable. Green and red
  gates retain stdout/stderr for paged controller reads and useful startup errors; the 64 MiB
  retention cap explicitly reports truncation.

- **Worker progress and usage are more accurate.** Completed commands report on the next poll
  immediately; restarted workers promptly prove surviving runtimes before advertising capacity.
  Controller-session discard does not run repository Compose teardown. Codex whole-turn accounting
  reads cumulative native evidence when available instead of counting only the last model call.
  Controller Codex disables dynamic plugin additions, and generated-file guidance follows the
  stable caller prompt to improve cache reuse without altering unrelated host features.

- **Network isolation and recovery remain run-scoped.** Admitted modes and captures survive loop,
  fork, editor and controller execution. Kernel-protected host ranges and gateway lifecycles preserve
  unrelated concurrent boxes; active streams are not limited by admission timeouts. Ownership-checked
  recovery never silently widens access. Inspection distinguishes blocked destinations, direct
  gateway refusals, unknown observations and incomplete cleanup.

- **Credential and MCP brokering cover supported paths.** Supported provider API keys remain
  host-held behind per-run stand-ins under filtered networking, including peers, roles and fallbacks.
  Supported remote MCP credentials are brokered in open and filtered runs, including read-only
  sessions; unsupported shapes are identified explicitly. Offline runs omit remote MCP servers.
  Access-only credentials renew on the trusted host, while native sign-in storage retains each
  provider's refresh semantics.

- **Locked clients, explicit builds and worker-managed images.** Coop-managed images use pinned provider
  CLIs/adapters with native updaters disabled. Restricted project images require explicit reviewed
  host builds and immutable input/client-base proof. Managed image identities avoid cross-version
  replacement and support scoped cleanup. Controller jobs run the worker's base, never a job
  repository's Dockerfile. Project tool-version pins also apply from temporary directories.

- **Host, service and filesystem authority are stronger.** Compose uses private validated
  configuration, approved scope and exact-owned cleanup. Secret-file and custom-volume approvals
  bind actual storage authority; read-only sources must exist and unsafe propagation is refused.
  Host Git excludes repository-controlled execution settings; integrity checks also work with
  modern Git reference verification without disabling object or reference validation. Fork
  generations and network grants use hardlink anchors, not reusable filesystem metadata. Older
  grants need the documented migration or fresh approval.

- **Parallel forks share one canonical task queue.** Durable assignments and generation-bound
  candidates keep ownership and reviewed completion on the project queue instead of copied queues.
  Claims, leases, release/block transitions and decision history preserve interrupted work.
  Completion rejects unfinished checklists and unsafe no-change claims. Finalization removes only
  contained scratch, retains artifacts and updates state consistently; watch shows task work.

- **Loop completion, review and fallback preserve unfinished work.** Missing or rejected terminal
  actions receive bounded same-session correction. Review debt survives pauses/crashes; protected
  gate sources include declared wrappers. Reviewers consume verification handoffs and request
  missing checks. Authentication, quota and exhausted-credit failures rotate through eligible
  model/provider/account targets with bounded recovery and visible waits.

- **ACP continuity and native capabilities remain intact.** Automatic startup and live selectors
  honor admitted scope. Reopened threads restore recorded native conversations; warm switches reuse
  only compatible boxes. Cancellation and cleanup preserve exact network/session authority.
  Native roles retain declared capabilities, Gemini supports documented thinking levels, and pinned
  Grok tools/login renewal retain their native lifecycle. Usage, delegate and cache accounting
  distinguish evidence from unavailable figures.

- **Independent agent and complete-loop evaluations.** Preview-first `coop eval` includes
  core/queue starters and source-tree shell/Python/Node maintenance examples. Candidate inputs
  and hidden graders are frozen separately; finished work is graded offline without credentials.
  Errors, timeouts and missing trials stay in the requested denominator. Comparisons reject damaged
  evidence and guess no correspondence between multiple configurations. Complete matching runs show
  per-case changes and conservative fixed-suite repeat uncertainty, with explicit assumptions,
  case/repeat counts and an inconclusive result instead of an unsupported winner.

- **Disable ordinary native web tools during evaluations.** Eval candidates cannot use the
  supported clients' normal web-search tools. This is a trusted-client configuration boundary,
  not an adversarial network firewall.

- **CLI, installation and release checks are clearer.** Help, completion, eval discovery, recovery
  guidance and task/network reports describe accepted commands and useful next actions.
  Install and update verify archive checksums; the installer additionally verifies the checksum
  signature when Cosign is installed. Downloads include signed checksums and build provenance.

- **Remote-session structured output is enforced at the completion boundary.** A turn may carry a
  bounded JSON Schema plus the SHA-256 digest of its exact bytes; Coop persists and compiles that
  contract, instructs the model with it, and refuses to complete on malformed or schema-invalid
  output. Schema repairs stay in the same native session and allow three provider responses per
  semantic round. A caller that enables semantic validation receives up to three schema-valid,
  unpublished, digest-bound candidates, then accepts or rejects those exact bytes through an
  idempotent validation endpoint; schema repair does not spend that caller-review budget. Pending
  candidates survive daemon restart, rejection re-prompts the same logical/native turn, acceptance
  publishes only the stored candidate with a durable receipt, and exhaustion publishes no
  assistant message. Once a candidate is staged, provider-runtime teardown is separate retryable
  cleanup: a teardown error cannot hide the candidate, and startup plus the bounded janitor reap
  any leftover process, box, projected credentials, and services without making a validation
  decision or changing the candidate. Accept, reject, and awaiting-turn cancellation likewise reap
  before changing state; a cleanup failure returns a retryable service error while preserving the
  candidate, and the caller retries the still-current decision with a fresh idempotency key after
  runtime recovery.

- **Detached forks have one repository-owned worker-state format.** Current detached workers use
  generation-bound `owner-v2` records; stopped `owner-v1` records remain readable for safe cleanup.
  Coop no longer decodes, signals, rewrites, or partially cleans up headerless pre-v8 records.
  Start/recreate, merge, remove, and stop reject unsupported state before runtime or
  workspace effects while retaining the exact file as lifecycle authority. Stop detached forks
  with v8 before upgrading, or follow the verified process/container procedure in the
  [migration guide](MIGRATING.md#canonical-tasks-across-isolated-forks); never fabricate current
  ownership by prepending the header. Worker, cleanup-pending, reservation, stable-identity,
  exact-owner reap, and atomic-write behavior retain ownership checks. Detached startup hands the
  exact launched reservation from parent to child and publishes the child's PID/token before any child mutation,
  so a successful concurrent stop cannot be undone by a delayed child.

- **Task authority no longer carries an automatic pre-v8 cache migration.** Coop opens only the
  current durable registry at `~/.local/state/coop/task-leases/v1`; it removes the adoption flock,
  cross-volume record copier, staging tree, and rename path from every authority open. If that
  durable root is absent while the retired cache registry contains anything, Coop refuses before
  creating or changing either tree. Stop every older Coop process and migrate the whole directory
  using `MIGRATING.md`; a missing or empty retired root starts clean, while an existing durable root
  is authoritative without consulting the cache. Record formats, inode rechecks, completion trust,
  and current crash recovery are unchanged.

- **Audit reopen authority has one complete-history format.** Coop reads active v3 and
  non-authorizing pending v4 records only. It removes support for the unshipped v1/v2 descendant
  formats and the now-unused `tasks unblock --adopt-audit-head` adoption transaction; unsupported
  versions and unknown fields remain fail-closed instead of being ignored. Current blocked-rewrite
  rebasing, interrupted-unblock recovery, and single-use completion remain unchanged.

- **`coop init` targets the current scaffold only.** Re-init still fills missing current files and
  preserves every existing hook, symlink, and custom hooks path, but it no longer recognizes and
  rewrites pre-v8 hook bytes, root-anchored `.agent` ignore rules, or the retired `.agent/rules/`
  un-ignore. `MIGRATING.md` gives the one-time manual replacements for a direct upgrade from a
  version older than v8.

- **Fork session re-entry uses one exact hint.** Coop now reads only the current
  `.coop/session.<provider>.<account>` record. It no longer adopts provider-only fork records or
  guesses the latest Codex conversation by cwd; a missing or invalid hint starts fresh, and Codex
  records the uniquely new native ID after the run. Exact per-account resume and `--new` remain.

- **Task leases now have one host-only authority.** Coop no longer mirrors its flock and heartbeat
  into provider-visible `tmp/lease.lock` and `tmp/lease.json` files. Acquisition, contention,
  `busy`/`stalled` observation, crash recovery, completion receipts, and audit reopen all use the
  inode-verified registry under `~/.local/state/coop/task-leases/`; task `tmp/` is scratch only.

- **Fleet is removed in favor of direct detached fork loops.** Start each worker with `coop fork
  <name> <target|preset> --loop -d --tasks <path>`; use the canonical shared queue instead of copied
  queue slices, `coop tasks watch` for merged progress, `coop fork ls`/`logs -f` for fork state and output,
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

- **The security baseline is enforced locally and in CI.** Coop now builds with Go 1.27.1, runs a
  pinned `govulncheck` as part of the one `make check` recipe, and asks Dependabot to watch Go
  modules weekly. MCP configuration also rejects duplicate case-insensitive header names and
  competing inline/environment Authorization sources instead of producing provider-dependent auth.
  Every scoped adapter, including Claude as a direct command or nested peer/preset role, consumes
  one validated immutable snapshot per run, so no consumer can bypass that policy or race a later
  edit to the shared source. Shared and native config reads reject symlinks and special files,
  operate nonblocking on one validated descriptor, and stop at 4 MiB. A malformed, ambiguous,
  unreadable, or oversized file, or a `COOP_MCP_FILE` placed inside a directory Coop mounts
  wholesale, now fails before provider launch. Codex and Grok native TOML is also checked
  semantically: Coop removes only
  byte-preservable canonical `[mcp_servers.*]` tables and refuses dotted, quoted, inline, or other
  alternate declarations before changing any scoped agent home, leaving the configured shared
  `COOP_MCP_FILE` as the sole MCP authority. Remote sessions now cross the same canonical snapshot
  boundary before changing private state and fail the turn before child launch when an active MCP
  authority or adapter projection is invalid, instead of warning and silently answering without
  tools.

- **Remote ACP uses the editor's existing SSH transport instead of a Coop TCP protocol.** Zed
  Remote Development can run `coop acp <target>` beside the remote repository, while a custom
  agent may invoke that same stdio command through `ssh -T`. Coop therefore adds no
  unauthenticated listener, socket bridge, client lifecycle, or second network trust boundary.
  Plain `coop acp` starts automatically with the first signed-in provider; explicit targets and
  presets take precedence. The live Provider and Account selectors remain available after
  connection.

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

Maintenance scenarios are comparison examples, not official benchmark scores. Repeats do not
establish unseen-task performance.

Releases before 10.1.2 are in [the full changelog at v11.3.0](https://github.com/AndrewDryga/coop/blob/v11.3.0/CHANGELOG.md).
