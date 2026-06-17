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
	in := "# a fleet\n" +
		"perf codex .agent/TASKS.perf.md\n" +
		"deps gemini .agent/TASKS.deps.md\n" +
		"docs .agent/TASKS.docs.md\n\n" // agent omitted → claude
	got, err := parseFleet(in)
	if err != nil {
		t.Fatalf("parseFleet: %v", err)
	}
	want := []fleetEntry{
		{"perf", "codex", ".agent/TASKS.perf.md"},
		{"deps", "gemini", ".agent/TASKS.deps.md"},
		{"docs", "claude", ".agent/TASKS.docs.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseFleet = %v, want %v", got, want)
	}
	if _, err := parseFleet("perf"); err == nil {
		t.Error("parseFleet: want error when the tasks path is missing")
	}
	if _, err := parseFleet("perf codex"); err == nil {
		t.Error("parseFleet: want error when only an agent is given (no tasks path)")
	}
	if _, err := parseFleet("ls codex q.md"); err == nil {
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

// The legend line and the ## Example block both contain "[ ]"/"[E]" but aren't real
// tasks; fleet split must not turn them into phantom slice entries.
func TestFleetSplitIgnoresLegendAndExample(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "r")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	tasks := "# .agent/TASKS.md — the work queue.\n" +
		"# [ ] todo   [w] claimed   [x] done   [B] blocked\n\n" +
		"## Example\n\n- [E] sample task\n\n" +
		"## Active\n\n- [ ] a\n- [ ] b\n- [ ] c\n"
	if err := os.WriteFile(filepath.Join(repo, ".agent", "TASKS.md"), []byte(tasks), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	if code, err := a.fleetSplit([]string{"2"}); err != nil || code != 0 {
		t.Fatalf("fleetSplit = (%d, %v), want (0, nil)", code, err)
	}
	s1, _ := os.ReadFile(filepath.Join(repo, ".agent", "TASKS.slice1.md"))
	s2, _ := os.ReadFile(filepath.Join(repo, ".agent", "TASKS.slice2.md"))
	all := string(s1) + string(s2)
	for _, want := range []string{"[ ] a", "[ ] b", "[ ] c"} {
		if !strings.Contains(all, want) {
			t.Errorf("real task %q missing from slices:\n%s", want, all)
		}
	}
	if strings.Contains(all, "[ ] todo") {
		t.Errorf("legend line leaked into a slice as a task:\n%s", all)
	}
	if strings.Contains(all, "[E]") {
		t.Errorf("Example block leaked into a slice:\n%s", all)
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
	// It also writes .agent/fleet with each fork's explicit tasks path.
	fleet, _ := os.ReadFile(filepath.Join(repo, ".agent", "fleet"))
	if !strings.Contains(string(fleet), "slice1 claude .agent/TASKS.slice1.md") {
		t.Errorf(".agent/fleet = %q, want an explicit slice1 path line", fleet)
	}
	parsed, err := parseFleet(string(fleet))
	if err != nil || len(parsed) != 2 {
		t.Errorf("written .agent/fleet does not parse back: %v (%d entries)", err, len(parsed))
	}
}
