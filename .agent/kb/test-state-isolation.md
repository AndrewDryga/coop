---
name: test-state-isolation
description: every test binary that can reach Coop's per-user state calls teststate.Isolate in TestMain; Coop refuses the real roots in a test binary, children inherit the folder, and live suites opt back into the real config and network state
subsystem: development
sources: [internal/testutil/teststate/teststate.go, internal/forkspace/execution.go, internal/forkspace/gitview.go, internal/tasks/lease.go, internal/config/config.go, internal/config/runfiles.go, internal/box/network_admission.go, internal/box/serviceapproval.go, internal/cli/eval_cmd.go, internal/cli/session_cmd.go, internal/acpproxy/trace.go, internal/cli/provider_live_e2e_test.go, internal/testutil/liveprovider/contract.go, internal/box/reap.go, internal/cli/main_test.go, internal/sessionsvc/main_test.go, internal/sessionsvc/acp.go]
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

**Live suites opt back in.** The providerlivee2e parents and the acpe2e `TestMain` drive the
signed-in providers on purpose, so they set `teststate.LiveEnv` and point `XDG_CONFIG_HOME` at
`teststate.DeveloperConfigHome()` before `config.Load()`. The parents hand filtered children
`teststate.DeveloperStateHome()` (`hostStateHome`, the accounts suite's brokered child) without
exporting it, so their own state stays isolated. The network runtime recover test marks itself too,
because its operator may keep the trial store outside the temporary folder. The other live suites
stay isolated: ACP children qualify a throwaway state tree, and the box runtime suites run with
egress `none`.

**Leftovers.** A killed test binary leaves its folder. The daily temp sweep
(`internal/box/reap.go`) removes `coop-test-state-*` entries older than an hour, and the
`coop-test-task-leases-*` and `coop-gitview-*` folders that earlier builds' test binaries made for
each process; a test binary that runs past an hour while a real coop sweeps could lose its folder.
Registries report any non-record entry with its full path, except `.service-launch.lock`: builds of
September 15 to 23, 2026 kept that lock in each `<sha256>` registry folder (29,648 on one Mac, and
on Ryker workers that ran those builds), and reporting it would make filtered launches, `coop up`,
fork changes and ACP cleanup refuse in each of those repositories. `executionsInRegistry` skips that
one name until the owner has removed those old-layout folders; task
2026-10-17-drop-the-old-service-launch-lock-skip-after-the holds the cleanup and then drops the skip.

**Proving it.** Run the tests under `sandbox-exec` with a profile that denies `file-write*` on
the two real roots, then read the denials with `/usr/bin/log show --predicate 'sender == "Sandbox"'`.
Nothing reaches the real folders, and every attempted write is named with its process.

## Changelog
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
