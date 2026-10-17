---
name: test-state-isolation
description: every test binary that can reach Coop's per-user state calls teststate.Isolate in TestMain; Coop refuses the real roots in a test binary, children inherit the folder, and live suites opt back into the real config and network state; the cli unit tests run with failing stubs for the host provider CLIs and runtimes
subsystem: development
sources: [internal/testutil/teststate/teststate.go, internal/forkspace/execution.go, internal/forkspace/service_lock_sweep.go, internal/cli/tempreap.go, internal/forkspace/gitview.go, internal/tasks/lease.go, internal/config/config.go, internal/config/runfiles.go, internal/box/network_admission.go, internal/box/serviceapproval.go, internal/cli/eval_cmd.go, internal/cli/session_cmd.go, internal/acpproxy/trace.go, internal/cli/provider_live_e2e_test.go, internal/testutil/liveprovider/contract.go, internal/box/reap.go, internal/cli/main_test.go, internal/cli/hostclis_test.go, internal/sessionsvc/main_test.go, internal/sessionsvc/acp.go, internal/box/isolated_mounts_test.go, internal/box/mounts_test.go]
updated: 2026-10-17
---

Coop's per-user state lives under the home folder: the execution registry with its
service-launch locks, the trusted git views, the task leases, the network authority, eval results
and sessions under `~/.local/state/coop`, and the config root with its private runfiles under
`~/.config/coop`. Test binaries that reached them wrote the developer's real folders: 57,925
service-launch locks on one Mac by 2026-10-16, about 130 leaked `coop-gitview-*` temp roots a day,
real settings read by `config.Load()`, and a run record a cli test sealed in the real network store
(`TestApprovedBuild` settling interrupted filtered runs with `XDG_STATE_HOME` unset).

**One helper, in every TestMain.** The rule is [[test-binaries-isolate-coop-state]], and
`TestStateTestBinariesRunUnderTeststate` names any package that forgets.
`internal/testutil/teststate.Isolate(m.Run)` makes one `coop-test-state-*` temp folder and points
`COOP_TEST_EXECUTION_REGISTRY_ROOT`, `COOP_GITVIEW_ROOT`, `COOP_TEST_LEASE_AUTHORITY_ROOT`,
`COOP_SERVICE_STATE_ROOT`, `XDG_CONFIG_HOME` and `XDG_STATE_HOME` inside it, then removes it when the
tests return. The 24 test binaries that link forkspace, config or tasks have the one-line
`TestMain` (cli and acpproxy wrap their fixture-binary setup in it; `teststate.Dir()` holds cli's
binary cache). The helper repeats the variable names because forkspace's and tasks' own tests call
it, and it stays a leaf even in its own tests: the local check scopes count test imports, so one
Coop import there made every caller a reverse consumer of tasks, agent and runtime.

**The guard is the refusal.** In a binary whose name ends in `.test`, `fallbackExecutionDir`
(registry and service locks), `gitViewRoot` (no more per-process temp root) and
`leaseAuthorityRoot` fail when their variable is unset, naming it and the helper.
`config.CheckTestFolder` guards the roots that follow a variable a developer's shell may set too:
`config.RootDir` (`XDG_CONFIG_HOME`; behind `Load`, `PrivateRunfiles(nil)`, `ExistingRunfiles(nil)`
and acpproxy's trace folder), `config.StateHome` (`XDG_STATE_HOME`; behind `box.NetworkStatePath`
on every launch and the eval results root) and `defaultSessionStateRoot` (HOME, since sessions
ignore `XDG_STATE_HOME`). Such a folder must lie inside the temporary folder, where teststate's
folder and every `t.TempDir` live; the one teststate started in counts too, for a test that moves
TMPDIR. Anything else may be a developer's export, inside HOME or not. A package that forgets fails
at the first touch instead of writing. A snapshot of the real folders cannot be the guard: the
owner's real coop processes write runfiles, views and locks all day.

**Children.** `COOP_TEST_STATE_ROOT` names the folder. A process the binary starts for itself with
the inherited environment keeps every root (lease helpers rely on sharing one registry); one started
with a clean environment gets its own folder, which leaks if the child is killed. The session
service strips every `COOP_` variable but `COOP_TEST_SESSION_*` from its provider child, so
sessionsvc's `TestMain` runs that helper (`COOP_TEST_SESSION_CHILD=1`) without a folder; it touches
no Coop state, and the refusals would fail it if it did.

**XDG folders.** Isolate replaces an inherited `XDG_CONFIG_HOME` or `XDG_STATE_HOME` wherever it
points. The one exception is a child of the live harness: `internal/testutil/liveprovider` puts
`teststate.LiveEnv` (`COOP_TEST_LIVE_HOMES=1`) in every environment it builds, so the child keeps
the folders of its layout, and the host's network state when granted, and the guards accept them.
When the helper replaces `XDG_CONFIG_HOME`, it pins `GOENV` to where `go` would have looked, so
fixture builds on Linux keep the developer's go settings.

**TMPDIR stays the developer's.** Box tests bind unix sockets under TMPDIR, within Darwin's 104-byte
socket path limit: the full path of `coop-ipc-*/control.sock` is 81 bytes under the real 49-byte
TMPDIR. A TMPDIR inside the state folder adds 31, and the bind fails with `invalid argument`. So a
test that writes into TMPDIR pins its own instead. Box's source copies no longer fall back to
TMPDIR: the copy helpers refuse an empty parent, and their tests pass `copyParent(t)`, a test folder.

**Live suites opt back in.** The providerlivee2e parents and the acpe2e `TestMain` drive the
signed-in providers on purpose, so they set `teststate.LiveEnv` and point `XDG_CONFIG_HOME` at
`teststate.DeveloperConfigHome()` before `config.Load()`. The parents hand filtered children
`teststate.DeveloperStateHome()` (`hostStateHome`, the accounts suite's brokered child) without
exporting it, so their own state stays isolated. The network runtime recover test marks itself too,
because its operator may keep the trial store outside the temporary folder. The other live suites
stay isolated: ACP children qualify a throwaway state tree, and the box runtime suites run with
egress `none`.

**Host programs.** The cli binary's untagged tests also stay off the developer's provider CLIs and
container runtime. `guardHostCLIs` (`internal/cli/hostclis_test.go`) puts a failing stub for codex,
claude, gemini, grok, gcloud (the Gemini usage report's token), docker and Apple's container first
on PATH; a stub records its call, and the binary fails after its tests, listing the calls. Before
it, three `coop models` tests ran the real `codex debug models` (which reads `~/.codex`) and
`docker info`, and every `coop init` test left runtime detection to the host: Apple's container
added a warning to the approved transcripts, and with both runtimes installed detection ran the
real `docker info`. A test takes a seam instead: `modelsApp` (an empty PATH and a failing
`a.acpModels`), `initAppOnDocker` (`rt` and `rtSet`), or a PATH of its own stubs, as
`TestInitDetectsTheRuntimeWhenNoneIsChosen` uses for init's own detection. The container stub keeps
detection visible: with both runtimes on PATH it asks Docker's daemon, so the guard sees it on
every machine. A helper child started with the state
folder shares the parent's stubs, and the parent reports. The tagged suites build without the guard
(`hostclis_tagged_test.go`). The report names calls, not tests; narrow it with `-run`.

**Leftovers.** A killed test binary leaves its folder. The daily temp sweep
(`internal/box/reap.go`) removes `coop-test-state-*` entries older than an hour, and the
`coop-test-task-leases-*` and `coop-gitview-*` folders that earlier builds' test binaries made for
each process; a test binary that runs past an hour while a real coop sweeps could lose its folder.
Registries report any non-record entry with its full path, and that refusal stops filtered launches,
`coop up`, fork changes and ACP cleanup in the repository. That includes the `.service-launch.lock`
that builds of September 15 to 23, 2026 kept in each `<sha256>` registry folder: 29,648 on one Mac
and one on a Ryker worker were removed on 2026-10-17, and docs/session-api.md tells an operator how
to remove any left. The locks in `service-locks/` piled up too (62,230 on that Mac, about 5,400 a
day until test binaries were kept out on 2026-10-17; real launches still make one per throwaway
repository, such as each doctor fixture or eval trial). Each lock now records its repository's
resolved path, and the daily temp sweep (`forkspace.SweepServiceLaunchLocks`, at most 10,000 locks
a pass) removes, while holding it exclusively without waiting, a lock whose path is gone or is now
another folder, and an unnamed one older than a day. `LockServiceLaunch` takes its lock again when
the file it locked is no longer the one at the path, so a removal never splits one lock in two.

**Proving it.** Run the tests under `sandbox-exec` with a profile that denies `file-write*` on
the two real roots, then read the denials with `/usr/bin/log show --predicate 'sender == "Sandbox"'`.
Nothing reaches the real folders, and every attempted write is named with its process.

## Changelog
- 2026-10-17 — service-launch locks record their repository and the daily sweep removes those of
  gone repositories (task 2026-10-17-stop-service-launch-lock-files-piling-up-in-the).
- 2026-10-17 — box's source-copy helpers lost their TMPDIR fallback, so their tests no longer pin
  TMPDIR: they pass `copyParent(t)`, which keeps pinTemp's leftover check on that folder.
- 2026-10-17 — the old-layout `.service-launch.lock` skip is gone after the owner-approved cleanup;
  the still-growing `service-locks/` folder is queued.
- 2026-10-17 — why Isolate leaves TMPDIR alone: with TMPDIR as long as one inside the state folder,
  `TestIsolatedMountRefusesSocketsAndKernelSources` could not bind its socket; with the real one it
  passes. Box's source-copy tests pin their own with `pinTemp`.
- 2026-10-17 — the cli binary's untagged tests run under `guardHostCLIs`. A discovery run with
  recording stubs first on PATH found `codex debug models` and `docker info` from three
  `coop models` tests, and `docker info` from detection in every `coop init` test once Apple's
  container was present; they now use `modelsApp` and `initAppOnDocker`, and init's detection with
  no runtime chosen has its own PATH-stub test. gcloud is stubbed too. Negative control: the old
  shapes pass alone and the binary fails, listing their calls.
- 2026-10-17 — review round: one live mark (`teststate.LiveEnv`, set by the live harness) replaced
  the HOME-based keep rule, so Isolate replaces every other inherited XDG folder;
  `config.CheckTestFolder` accepts only temporary folders, for config, state and sessions alike;
  the service-state root is isolated; the sweep keeps the old lease prefix and adds `coop-gitview-`;
  the "every TestMain" verdict moved to [[test-binaries-isolate-coop-state]]; the owner's cleanup
  command now lives only in the blocked task.
- 2026-10-17 — `XDG_STATE_HOME` isolated too, after a cli test sealed a record in the real network
  store: `config.StateHome` refuses it unset or set to `$HOME/.local/state`, the sessions root
  refuses a non-temporary HOME, and both XDG folders follow one keep-or-replace rule.
- 2026-10-17 — the `.service-launch.lock` skip stays: dropping it would refuse work in every
  repository with a September lock until the owner's cleanup, so its removal moved to task
  2026-10-17-drop-the-old-service-launch-lock-skip-after-the.
- 2026-10-16 — created with the shared helper and the refusals. Before: main's offender tests under
  the sandbox tried to write the real registry and failed; after: no denials (see the task's log).
