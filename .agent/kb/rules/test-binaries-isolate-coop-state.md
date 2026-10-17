---
name: test-binaries-isolate-coop-state
description: "every test binary that links config, forkspace or tasks runs under teststate.Isolate; a test never points Coop at the developer's real folders"
scope: architecture
sources: [internal/testutil/teststate/teststate.go, internal/config/config.go, internal/teststate_check_test.go, internal/testutil/liveprovider/contract.go]
check: "go test ./internal -run 'TestStateTestBinariesRunUnderTeststate|TestMissingTeststateFollowsOnlyRealLinks'"
updated: 2026-10-17
---

# Every test binary that can reach Coop's state runs under teststate

A package whose test binary links `internal/config`, `internal/forkspace` or `internal/tasks`,
through its own code, its tests or a dependency, has
`func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m.Run)) }`, wrapping any setup of its own
inside that call. Within a test, point Coop's roots only at folders the test owns (`t.TempDir()`,
`teststate.Dir()`), never at `~/.config`, `~/.local/state` or a folder a developer exported.

**Why:** test binaries wrote the owner's real state: 57,925 service-launch locks on one Mac by
2026-10-16, run files in the real config root, and a run record a cli test sealed in the real
network store on 2026-10-17. A snapshot of the real folders cannot catch that, because the owner's
real coop writes there all day; Coop has to refuse.

**How to apply:**
- A new test package that links one of the three gets the one-line TestMain; the check names any
  package that forgets.
- In a test binary Coop refuses the registry, view and lease roots when their `COOP_TEST_*`
  variable is unset, the config and state homes outside the temporary folder
  (`config.CheckTestFolder`), and the default sessions root unless HOME is temporary. A package that
  forgets Isolate fails at the first touch instead of writing.
- A live provider suite that needs the developer's signed-in config sets `teststate.LiveEnv` and
  reads `teststate.DeveloperConfigHome()` or `DeveloperStateHome()`; the live harness marks every
  child it starts. Nothing else sets the mark, apart from a live test whose operator names its trial
  store.
- The check cannot see fixture programs under `testdata`. Background: [[test-state-isolation]].

## Changelog
- 2026-10-17 — created from the review of task
  2026-10-15-keep-test-binaries-out-of-the-real-coop-state-an. Swept the tree with the check: 24
  test binaries link the three packages, and one lacked Isolate (tools/lifecyclebench, added on
  main the same week; fixed here). Negative control: dropping its TestMain makes the check name it.
