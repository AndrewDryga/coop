package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestParseFleet(t *testing.T) {
	in := "# a fleet\nperf codex\ndeps gemini\ndocs\n\n"
	got, err := parseFleet(in)
	if err != nil {
		t.Fatalf("parseFleet: %v", err)
	}
	want := []fleetEntry{{"perf", "codex"}, {"deps", "gemini"}, {"docs", "claude"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseFleet = %v, want %v", got, want)
	}
	if _, err := parseFleet("perf bogus"); err == nil {
		t.Error("parseFleet: want error for unknown agent")
	}
	if _, err := parseFleet("ls codex"); err == nil {
		t.Error("parseFleet: want error for reserved name")
	}
}

func TestPolicyScan(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	ws, err := setupFork(repo, "x")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "safe.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "add files")
	if err := gitFetchInto(repo, ws, "x"); err != nil {
		t.Fatal(err)
	}
	warns := strings.Join(policyScan(repo, "review/x"), "\n")
	if !strings.Contains(warns, ".env") {
		t.Errorf("policyScan missed .env: %q", warns)
	}
	if strings.Contains(warns, "safe.txt") {
		t.Errorf("policyScan wrongly flagged safe.txt: %q", warns)
	}
}

func TestFleetSplit(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "r")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "TASKS.md"),
		[]byte("- [ ] a\n- [ ] b\n- [ ] c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	if code, err := a.fleetSplit([]string{"2"}); err != nil || code != 0 {
		t.Fatalf("fleetSplit = (%d, %v), want (0, nil)", code, err)
	}
	// Round-robin: slice1 gets a + c, slice2 gets b.
	s1, _ := os.ReadFile(filepath.Join(repo, ".agent", "TASKS.slice1.md"))
	s2, _ := os.ReadFile(filepath.Join(repo, ".agent", "TASKS.slice2.md"))
	if !strings.Contains(string(s1), "[ ] a") || !strings.Contains(string(s1), "[ ] c") {
		t.Errorf("slice1 = %q, want a and c", s1)
	}
	if !strings.Contains(string(s2), "[ ] b") {
		t.Errorf("slice2 = %q, want b", s2)
	}
}
