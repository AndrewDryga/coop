// Package teststate keeps a test binary out of the developer's real Coop state.
//
// Coop keeps per-user state under the home folder: the execution registry with its service-launch
// locks, the git views, the task leases and the network authority under ~/.local/state/coop, and
// the config root with its private runfiles under ~/.config/coop. Test binaries that reached those
// roots wrote the developer's real folders; by October 2026 one Mac held 57,925 service-launch
// locks, most of them left by tests, and a cli test sealed a run record in the real network store.
// Every package whose tests can reach that state calls Isolate from its TestMain, which points each
// root at a folder inside one temporary directory and removes it at the end. Coop itself refuses
// those roots in a test binary that did not name them, so a package that forgets fails instead of
// writing the real ones.
package teststate

import (
	"fmt"
	"os"
	"path/filepath"
)

// rootEnv names the folder Isolate made. A process the test binary starts for itself inherits it,
// and with it every root its parent chose.
const rootEnv = "COOP_TEST_STATE_ROOT"

// LiveEnv, set to 1, marks a live provider run, which uses chosen XDG folders on purpose: a suite's
// parent loads the developer's signed-in config, and the live harness (internal/testutil/
// liveprovider) hands each child the folders of its own layout, and maybe this host's network
// state. Isolate keeps a marked child's XDG folders, and Coop's test guards accept them. Coop's
// config package repeats the name, because it cannot import this one.
const LiveEnv = "COOP_TEST_LIVE_HOMES"

// The XDG folders Coop reads like every XDG program does: the config root and its runfiles live
// under the first, the network authority and eval results under the second.
const (
	configHomeEnv = "XDG_CONFIG_HOME"
	stateHomeEnv  = "XDG_STATE_HOME"
)

// roots are the variables Coop reads for its state roots in a test binary, and the folder each one
// gets. The names repeat the constants in forkspace, tasks and box because their own tests call
// Isolate, and importing those packages here would be an import cycle. A name that drifts fails
// loudly: Coop refuses an unnamed root in every test binary.
var roots = []struct{ env, dir string }{
	{"COOP_TEST_EXECUTION_REGISTRY_ROOT", "executions"},
	{"COOP_GITVIEW_ROOT", "gitviews"},
	{"COOP_TEST_LEASE_AUTHORITY_ROOT", "task-leases"},
	{"COOP_SERVICE_STATE_ROOT", "service-state"},
}

// xdgHomes are the XDG folders Isolate replaces, each with the folder it gets.
var xdgHomes = []struct{ env, dir string }{
	{configHomeEnv, "config"},
	{stateHomeEnv, "state"},
}

// developerHomes holds each XDG folder as this process started, before Isolate replaced it.
var developerHomes = map[string]string{}

// Isolate runs tests, normally m.Run, with Coop's state in a new temporary folder, then removes the
// folder and returns the tests' exit code for os.Exit.
//
// It replaces an XDG_CONFIG_HOME or XDG_STATE_HOME the binary inherited, wherever it points: one
// exported in a developer's shell names the real folders. Only a child the live harness marked with
// LiveEnv keeps the folders the harness chose for it.
func Isolate(tests func() int) int {
	if os.Getenv(rootEnv) != "" {
		return tests()
	}
	root, err := os.MkdirTemp("", "coop-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "teststate:", err)
		return 1
	}
	defer os.RemoveAll(root)
	env := map[string]string{rootEnv: root}
	for _, r := range roots {
		env[r.env] = filepath.Join(root, r.dir)
	}
	live := os.Getenv(LiveEnv) != ""
	for _, x := range xdgHomes {
		developerHomes[x.env] = os.Getenv(x.env)
		if !live {
			env[x.env] = filepath.Join(root, x.dir)
		}
	}
	// On Linux the go command finds its own settings under XDG_CONFIG_HOME. Tests build fixture
	// binaries with it, so it keeps reading the developer's settings.
	if _, replaced := env[configHomeEnv]; replaced && os.Getenv("GOENV") == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			env["GOENV"] = filepath.Join(dir, "go", "env")
		}
	}
	for key, value := range env {
		if key != rootEnv && key != "GOENV" {
			if err := os.MkdirAll(value, 0o700); err != nil {
				fmt.Fprintln(os.Stderr, "teststate:", err)
				return 1
			}
		}
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, "teststate:", err)
			return 1
		}
	}
	return tests()
}

// Dir is the folder Isolate made for this test binary, for other files that should live and go
// with it, or "" outside Isolate.
func Dir() string { return os.Getenv(rootEnv) }

// DeveloperConfigHome is the XDG_CONFIG_HOME Coop uses outside tests: the one this process started
// with, else ~/.config. Only the live provider suites use it, to load the signed-in configuration
// they exercise on purpose.
func DeveloperConfigHome() string { return developerHome(configHomeEnv, ".config") }

// DeveloperStateHome is the XDG_STATE_HOME Coop uses outside tests: the one this process started
// with, else ~/.local/state. Only the live provider suites use it, to hand their filtered children
// the network qualification `coop net setup` made on this host.
func DeveloperStateHome() string {
	return developerHome(stateHomeEnv, filepath.Join(".local", "state"))
}

func developerHome(env, fallback string) string {
	if value := developerHomes[env]; value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, fallback)
}
