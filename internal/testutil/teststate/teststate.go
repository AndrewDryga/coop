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
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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
	homeEnv       = "HOME"
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
	developerHomes[homeEnv] = os.Getenv(homeEnv)
	// HOME bounds the .coopignore files a project takes from the folders above it: Coop reads them
	// up to the home folder. A test's project lives in the temporary folder, outside the developer's
	// home, so the walk climbed to / and a stray .coopignore anywhere above the temporary folder, as
	// in /tmp on a CI runner, changed what the tests saw. A home inside this folder puts the
	// temporary folder above it, where the walk stops. A home that holds the temporary folder, or
	// lies inside it, bounds the walk by itself: that is a home a test chose, as for a process it
	// starts as a fixture (a fake docker running this binary again), which checks it keeps it.
	if !live && !nestsWithTemp(os.Getenv(homeEnv)) {
		env[homeEnv] = filepath.Join(root, "home")
	}
	// The go command finds its settings and caches under the home folder and, on Linux,
	// XDG_CONFIG_HOME. Tests build fixture binaries with it, so it keeps the developer's. Docker is
	// left alone: Coop's own Docker code must see the environment a person's coop sees, and with no
	// config folder of its own the docker CLI reaches the default socket.
	for key, dir := range toolFolders() {
		if os.Getenv(key) == "" {
			env[key] = dir
		}
	}
	for key, value := range env {
		if _, tool := toolFolderEnvs[key]; key != rootEnv && !tool {
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
	if err := os.Setenv("PATH", developerToolsPath()); err != nil {
		fmt.Fprintln(os.Stderr, "teststate:", err)
		return 1
	}
	return tests()
}

// gitStub is the git macOS installs in /usr/bin: a stub that asks xcrun where the developer tools
// are on every call before it runs the real git, about 6 ms a call against 3 for the real one. The
// tests run tens of thousands of git commands, a session review alone some 240.
const gitStub = "/usr/bin/git"

// developerToolsPath is PATH with the folder of the git the stub runs just ahead of /usr/bin, when
// PATH finds the stub, so the tests' git runs without it. A program found before /usr/bin is still
// found first, and a name /usr/bin forwards to the developer tools reaches the same program. Not a
// link to git: Apple's git looks for its helpers and templates beside the path it was started from.
func developerToolsPath() string {
	path := os.Getenv("PATH")
	if runtime.GOOS != "darwin" {
		return path
	}
	if git, err := exec.LookPath("git"); err != nil || git != gitStub {
		return path
	}
	out, err := exec.Command("/usr/bin/xcrun", "--find", "git").Output()
	tools := filepath.Dir(strings.TrimSpace(string(out)))
	if err != nil || !filepath.IsAbs(tools) || tools == filepath.Dir(gitStub) {
		return path
	}
	entries := filepath.SplitList(path)
	for i, entry := range entries {
		if filepath.Clean(entry) == filepath.Dir(gitStub) {
			return strings.Join(slices.Insert(entries, i, tools), string(os.PathListSeparator))
		}
	}
	return path
}

// Dir is the folder Isolate made for this test binary, for other files that should live and go
// with it, or "" outside Isolate.
func Dir() string { return os.Getenv(rootEnv) }

// DeveloperHome is the home folder this process started with, before Isolate moved HOME: the folder
// a test means when it checks that Coop stays out of the developer's real one.
func DeveloperHome() string {
	if home := developerHomes[homeEnv]; home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return home
}

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
	home := developerHomes[homeEnv]
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return ""
		}
	}
	return filepath.Join(home, fallback)
}

// nestsWithTemp reports whether home holds the temporary folder or lies inside it, however either
// is spelled through links.
func nestsWithTemp(home string) bool {
	if home == "" || !filepath.IsAbs(home) {
		return false
	}
	spellings := func(path string) []string {
		out := []string{filepath.Clean(path)}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			out = append(out, resolved)
		}
		return out
	}
	within := func(dir, path string) bool {
		rel, err := filepath.Rel(dir, path)
		return err == nil && (rel == "." || filepath.IsLocal(rel))
	}
	for _, h := range spellings(home) {
		for _, tmp := range spellings(os.TempDir()) {
			if within(h, tmp) || within(tmp, h) {
				return true
			}
		}
	}
	return false
}

// toolFolderEnvs are the variables that name the go command's folders.
var toolFolderEnvs = map[string]bool{"GOENV": true, "GOCACHE": true, "GOPATH": true}

// toolFolders are the folders the go command uses by default, worked out before Isolate moves HOME
// and XDG_CONFIG_HOME, as the go command works them out.
func toolFolders() map[string]string {
	folders := map[string]string{}
	if dir, err := os.UserConfigDir(); err == nil {
		folders["GOENV"] = filepath.Join(dir, "go", "env")
	}
	if dir, err := os.UserCacheDir(); err == nil {
		folders["GOCACHE"] = filepath.Join(dir, "go-build")
	}
	if home, err := os.UserHomeDir(); err == nil {
		folders["GOPATH"] = filepath.Join(home, "go")
	}
	return folders
}
