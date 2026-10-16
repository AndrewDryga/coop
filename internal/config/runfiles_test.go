package config

import (
	"os"
	"path/filepath"
	"testing"
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
