package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

// The daily sweep looks for the runfiles folder without creating it, and finds the one a run made.
func TestExistingRunfilesIsReadOnlyAndFindsTheCreatedFolder(t *testing.T) {
	cfg := &Config{BoxHome: filepath.Join(t.TempDir(), "box-home")}
	if got, exists, err := ExistingRunfiles(cfg); err != nil || exists || got != "" {
		t.Fatalf("locate absent runfiles = (%q, %v, %v), want absent", got, exists, err)
	}
	if _, err := os.Lstat(cfg.BoxHome); !os.IsNotExist(err) {
		t.Fatalf("a read-only lookup created state: %v", err)
	}
	created, err := PrivateRunfiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertPerm(t, filepath.Join(cfg.BoxHome, "runfiles"), 0o700)
	got, exists, err := ExistingRunfiles(cfg)
	if err != nil || !exists || got != created {
		t.Fatalf("locate created runfiles = (%q, %v, %v), want (%q, true, nil)", got, exists, err, created)
	}
}

// The container runtime has no Config when it makes a Docker client folder. Its folder must be the
// one the daily sweep visits for the Config every command loads, or killed runs' folders would
// pile up where nothing looks.
func TestRunfilesWithoutAConfigIsTheLoadedConfigs(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	made, err := PrivateRunfiles(nil)
	if err != nil {
		t.Fatal(err)
	}
	if swept, exists, err := ExistingRunfiles(cfg); err != nil || !exists || swept != made {
		t.Fatalf("the sweep looks in (%q, %v, %v); the runtime made %q", swept, exists, err, made)
	}
}

// The Coop home may be a link, as dotfiles setups make it. The runfiles folder below it may not:
// a link there would let whoever placed it choose where Coop keeps a run's credentials and Docker
// client configs. A folder others could open is closed again before use.
func TestRunfilesFollowsALinkedHomeButNotALinkedFolder(t *testing.T) {
	target := t.TempDir()
	home := filepath.Join(t.TempDir(), "coop")
	if err := os.Symlink(target, home); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "runfiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(target, "runfiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := PrivateRunfiles(&Config{BoxHome: home})
	if err != nil {
		t.Fatalf("a linked Coop home was refused: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(target, "runfiles"))
	if err != nil || root != want {
		t.Fatalf("runfiles = %q, want the link's target %q (%v)", root, want, err)
	}
	assertPerm(t, want, 0o700)

	linked := &Config{BoxHome: t.TempDir()}
	if err := os.Symlink(t.TempDir(), filepath.Join(linked.BoxHome, "runfiles")); err != nil {
		t.Fatal(err)
	}
	if got, err := PrivateRunfiles(linked); err == nil {
		t.Fatalf("a linked runfiles folder was used: %q", got)
	}
	if got, exists, err := ExistingRunfiles(linked); err == nil || exists {
		t.Fatalf("a linked runfiles folder was located: (%q, %v, %v)", got, exists, err)
	}
}

// A test binary that names no config home must fail, never use the developer's real one: tests that
// loaded it read the real settings and left run files in it.
func TestConfigRootRefusesATestBinaryWithoutItsOwnConfigHome(t *testing.T) {
	clearAgentEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if root, err := RootDir(); err == nil || !strings.Contains(err.Error(), "XDG_CONFIG_HOME") {
		t.Fatalf("RootDir = %q, %v; want a refusal naming XDG_CONFIG_HOME", root, err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("Load read the real config without the test's own config home")
	}
	if _, err := PrivateRunfiles(nil); err == nil {
		t.Fatal("PrivateRunfiles made run files in the real config home")
	}
	if _, _, err := ExistingRunfiles(nil); err == nil {
		t.Fatal("ExistingRunfiles looked in the real config home")
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("the refused calls wrote under HOME: %v, %v", entries, err)
	}
}

// In a test binary a config or state home counts only inside the temporary folder that holds the
// binary's teststate folder: a developer's shell may export one anywhere, inside HOME or not, and a
// cli test that fell back to ~/.local/state sealed a run record in the real network store. A marked
// live provider run chooses its own. The guard reads names only, so nothing here opens a folder.
func TestStateHomeRefusesAnyFolderButTheTestsOwn(t *testing.T) {
	home := teststate.DeveloperHome()
	real := filepath.Join(home, ".local", "state")
	elsewhere := filepath.Join(string(filepath.Separator), "data", "state")
	for _, value := range []string{"", real, elsewhere, "state"} {
		t.Setenv("XDG_STATE_HOME", value)
		if got, err := StateHome(); err == nil || !strings.Contains(err.Error(), "XDG_STATE_HOME") {
			t.Errorf("XDG_STATE_HOME=%q: StateHome = %q, %v; want a refusal naming XDG_STATE_HOME", value, got, err)
		}
	}
	if err := CheckTestFolder("HOME", home); err == nil {
		t.Error("the developer's HOME passed as a test's own")
	}
	own := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", own)
	if got, err := StateHome(); err != nil || got != own {
		t.Fatalf("StateHome = %q, %v; want the test's own %s", got, err, own)
	}
	// A test that moves TMPDIR keeps the folders it made before: teststate's folder remembers where
	// the temporary folder was. Without either, the same folder is refused.
	t.Setenv("TMPDIR", t.TempDir())
	if got, err := StateHome(); err != nil || got != own {
		t.Errorf("StateHome = %q, %v after TMPDIR moved; want the test's own %s", got, err, own)
	}
	t.Setenv(testStateRootEnv, "")
	if got, err := StateHome(); err == nil {
		t.Errorf("StateHome = %q, in neither temporary folder; want a refusal", got)
	}
	t.Setenv(testLiveEnv, "1")
	t.Setenv("XDG_STATE_HOME", real)
	if got, err := StateHome(); err != nil || got != real {
		t.Errorf("StateHome = %q, %v in a live run; want the state it chose, %s", got, err, real)
	}
}
