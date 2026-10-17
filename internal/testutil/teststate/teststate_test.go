package teststate

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// This test file imports no Coop package: the helper is a leaf for every package that calls it, and
// the local check scopes count test imports. A root name that drifts from the one Coop reads fails
// loudly anyway, because Coop refuses an unnamed root in every test binary.
func TestMain(m *testing.M) { os.Exit(Isolate(m.Run)) }

// A process the test binary starts for itself inherits the folder and keeps every root in it.
func TestIsolateKeepsTheRootsOfTheProcessThatStartedIt(t *testing.T) {
	before := os.Environ()
	ran := false
	if code := Isolate(func() int { ran = true; return 7 }); code != 7 || !ran {
		t.Fatalf("Isolate = %d, ran %t; want the tests' own exit code", code, ran)
	}
	if after := os.Environ(); !slices.Equal(before, after) {
		t.Fatal("a child changed the roots its parent chose")
	}
}

// freshBinary makes the next Isolate act like a binary nobody started for itself, and puts every
// variable Isolate sets back when the test ends, with the developer folders it remembered.
func freshBinary(t *testing.T) {
	t.Helper()
	saved := maps.Clone(developerHomes)
	t.Cleanup(func() { developerHomes = saved })
	t.Setenv(rootEnv, "")
	t.Setenv(LiveEnv, "")
	t.Setenv(homeEnv, os.Getenv(homeEnv))
	for key := range toolFolderEnvs {
		t.Setenv(key, "")
	}
	for _, r := range roots {
		t.Setenv(r.env, os.Getenv(r.env))
	}
	for _, x := range xdgHomes {
		t.Setenv(x.env, "")
	}
}

// A fresh binary gets every root inside one new folder, gone once the tests return.
func TestIsolateMakesOneFolderAndRemovesIt(t *testing.T) {
	freshBinary(t)
	for _, r := range roots {
		t.Setenv(r.env, filepath.Join(t.TempDir(), "stale"))
	}
	home, _ := os.UserHomeDir()
	tools := toolFolders()
	var made string
	Isolate(func() int {
		made = Dir()
		for _, r := range append(roots, xdgHomes...) {
			if got := os.Getenv(r.env); got != filepath.Join(made, r.dir) {
				t.Errorf("%s = %s, want a folder inside %s", r.env, got, made)
			}
		}
		// The home folder moves too, so the .coopignore files above a test's project stop at the
		// temporary folder; the go command and Docker keep the developer's folders.
		if got := os.Getenv(homeEnv); got != filepath.Join(made, "home") {
			t.Errorf("HOME = %s, want a folder inside %s", got, made)
		}
		for key, want := range tools {
			if got := os.Getenv(key); got != want {
				t.Errorf("%s = %q, want the developer's %q", key, got, want)
			}
		}
		if DeveloperConfigHome() != filepath.Join(home, ".config") {
			t.Errorf("DeveloperConfigHome = %s, want ~/.config", DeveloperConfigHome())
		}
		if DeveloperStateHome() != filepath.Join(home, ".local", "state") {
			t.Errorf("DeveloperStateHome = %s, want ~/.local/state", DeveloperStateHome())
		}
		if !strings.HasSuffix(os.Getenv("GOENV"), filepath.Join("go", "env")) {
			t.Errorf("GOENV = %q; the go command would lose the developer's settings", os.Getenv("GOENV"))
		}
		return 0
	})
	if made == "" || !strings.HasPrefix(filepath.Base(made), "coop-test-state-") {
		t.Fatalf("Isolate made %q", made)
	}
	if _, err := os.Lstat(made); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the isolated folder outlived the tests: %v", err)
	}
}

// An inherited XDG folder is replaced wherever it points, the developer's own (remembered for the
// live suites) as much as a temporary one. Only a child the live harness marked keeps the folders
// the harness chose, the host's network state included. Only names are compared: nothing here
// opens the developer's folders.
func TestIsolateReplacesInheritedXDGFoldersUnlessALiveHarnessChoseThem(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	realConfig, realState, harness := filepath.Join(home, ".config"), filepath.Join(home, ".local", "state"), t.TempDir()

	freshBinary(t)
	t.Setenv(configHomeEnv, realConfig)
	t.Setenv(stateHomeEnv, harness)
	Isolate(func() int {
		for _, x := range xdgHomes {
			if got := os.Getenv(x.env); got != filepath.Join(Dir(), x.dir) {
				t.Errorf("%s = %s, want the inherited folder replaced by one inside %s", x.env, got, Dir())
			}
		}
		if DeveloperConfigHome() != realConfig || DeveloperStateHome() != harness {
			t.Errorf("DeveloperConfigHome = %s, DeveloperStateHome = %s; want the inherited ones", DeveloperConfigHome(), DeveloperStateHome())
		}
		return 0
	})

	freshBinary(t)
	t.Setenv(LiveEnv, "1")
	t.Setenv(configHomeEnv, harness)
	t.Setenv(stateHomeEnv, realState)
	Isolate(func() int {
		if config, state := os.Getenv(configHomeEnv), os.Getenv(stateHomeEnv); config != harness || state != realState {
			t.Errorf("XDG_CONFIG_HOME = %s, XDG_STATE_HOME = %s; want the live harness's %s and the host's %s kept", config, state, harness, realState)
		}
		return 0
	})
}
