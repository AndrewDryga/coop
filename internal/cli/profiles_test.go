package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestCmdProfiles(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	// claude: a signed-in "work" profile (cred file present) + an unsigned "personal".
	work := cfg.AgentProfileDir("claude", "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".credentials.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.AgentProfileDir("claude", "personal"), 0o700); err != nil {
		t.Fatal(err)
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, err := (&app{cfg: cfg}).cmdProfiles([]string{"claude"})
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if code != 0 || err != nil {
		t.Fatalf("cmdProfiles: code=%d err=%v", code, err)
	}
	for _, want := range []string{"work", "signed in", "personal", "not signed in"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCmdProfilesUnknownAgent(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if code, err := a.cmdProfiles([]string{"nope"}); code != 2 || err == nil {
		t.Errorf("unknown agent: code=%d err=%v, want 2 + error", code, err)
	}
}

func TestCmdProfilesDefault(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	for _, p := range []string{"work", "personal"} {
		if err := os.MkdirAll(cfg.AgentProfileDir("claude", p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{cfg: cfg}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bad arity", []string{"default", "claude"}},
		{"unknown agent", []string{"default", "nope", "work"}},
		{"unknown profile", []string{"default", "claude", "ghost"}},
	} {
		if code, err := a.cmdProfiles(tc.args); code != 2 || err == nil {
			t.Errorf("%s: code=%d err=%v, want 2 + error", tc.name, code, err)
		}
	}

	// Set the default (discard the confirmation listing on stdout).
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, err := a.cmdProfiles([]string{"default", "claude", "personal"})
	_ = w.Close()
	os.Stdout = old
	_, _ = io.ReadAll(r)
	if code != 0 || err != nil {
		t.Fatalf("set default: code=%d err=%v", code, err)
	}
	if got := cfg.DefaultProfileOf("claude"); got != "personal" {
		t.Errorf("default = %q, want personal", got)
	}
}

func TestRemoveProfile(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	for _, p := range []string{"personal", "personal_backup", "default"} {
		if err := os.MkdirAll(cfg.AgentProfileDir("claude", p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.SetDefaultProfile("claude", "personal"); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bad arity", []string{"rm", "claude"}},
		{"unknown agent", []string{"rm", "nope", "default"}},
		{"unknown profile", []string{"rm", "claude", "ghost"}},
		{"refuses the marked default", []string{"rm", "claude", "personal"}},
	} {
		if code, err := a.cmdProfiles(tc.args); code != 2 || err == nil {
			t.Errorf("%s: code=%d err=%v, want 2 + error", tc.name, code, err)
		}
	}
	// personal (the default) must survive the refused deletion.
	if !pathExists(cfg.AgentProfileDir("claude", "personal")) {
		t.Fatal("refused deletion still removed the default profile dir")
	}

	// Remove the stray "default" profile (discard the confirmation listing on stdout).
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, err := a.cmdProfiles([]string{"rm", "claude", "default"})
	_ = w.Close()
	os.Stdout = old
	_, _ = io.ReadAll(r)
	if code != 0 || err != nil {
		t.Fatalf("rm default: code=%d err=%v", code, err)
	}
	if pathExists(cfg.AgentProfileDir("claude", "default")) {
		t.Error("default profile dir was not removed")
	}
	if !pathExists(cfg.AgentProfileDir("claude", "personal")) {
		t.Error("removing default wrongly affected personal")
	}
}
