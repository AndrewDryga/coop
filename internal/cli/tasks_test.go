package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// TestTaskQueuesMonorepo: with no --tasks/COOP_TASKS, taskQueues derives the queue set from
// .agent/project.yaml — a monorepo's subproject queues — while an explicit override still wins.
func TestTaskQueuesMonorepo(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("subprojects:\n  - runner\n  - packs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := taskQueues(&config.Config{}, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join("runner", ".agent", "tasks"), filepath.Join("packs", ".agent", "tasks")}
	if !slices.Equal(got, want) {
		t.Errorf("monorepo queues = %v, want %v", got, want)
	}

	// An explicit COOP_TASKS (cfg.TasksFiles) overrides project.yaml.
	got, _ = taskQueues(&config.Config{TasksFiles: []string{".agent/tasks"}}, repo, nil)
	if !slices.Equal(got, []string{".agent/tasks"}) {
		t.Errorf("COOP_TASKS override = %v, want [.agent/tasks]", got)
	}

	// A single repo (no project.yaml) still defaults to .agent/tasks.
	got, _ = taskQueues(&config.Config{}, t.TempDir(), nil)
	if !slices.Equal(got, []string{filepath.Join(".agent", "tasks")}) {
		t.Errorf("single-repo default = %v, want [.agent/tasks]", got)
	}
}

func TestExtractTasksFlags(t *testing.T) {
	flags, rest, err := extractTasksFlags([]string{"--tasks", "a", "list", "--tasks=b", "--debug"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(flags, []string{"a", "b"}) {
		t.Errorf("flags = %v, want [a b]", flags)
	}
	if !slices.Equal(rest, []string{"list", "--debug"}) {
		t.Errorf("rest = %v, want [list --debug]", rest)
	}

	// A value-bearing flag with no value is an error, not a silently-dropped flag.
	for _, bad := range [][]string{{"--tasks"}, {"list", "--tasks"}, {"--tasks="}, {"--tasks", "--debug"}} {
		if _, _, err := extractTasksFlags(bad); err == nil {
			t.Errorf("extractTasksFlags(%v) should error on a missing value", bad)
		}
	}
}

// TestFlagValue covers the shared value-bearing-flag parser: both forms, and the missing-value
// cases (trailing flag, flag-as-value, empty =).
func TestFlagValue(t *testing.T) {
	ok := func(args []string, wantVal string, wantN int) {
		t.Helper()
		v, n, found, err := flagValue(args, 0, "--f")
		if !found || err != nil || v != wantVal || n != wantN {
			t.Errorf("flagValue(%v) = (%q,%d,%v,%v), want (%q,%d,true,nil)", args, v, n, found, err, wantVal, wantN)
		}
	}
	ok([]string{"--f", "x"}, "x", 2)
	ok([]string{"--f=x"}, "x", 1)
	for _, bad := range [][]string{{"--f"}, {"--f", "-g"}, {"--f="}} {
		if _, _, found, err := flagValue(bad, 0, "--f"); !found || err == nil {
			t.Errorf("flagValue(%v) should be found with an error", bad)
		}
	}
	if _, _, found, _ := flagValue([]string{"other"}, 0, "--f"); found {
		t.Error("flagValue should not match an unrelated token")
	}
}

func TestTaskQueues(t *testing.T) {
	repo := t.TempDir()
	cfg := &config.Config{TasksFiles: []string{".agent/tasks"}}

	// No flags → the configured default (.agent/tasks).
	if got, err := taskQueues(cfg, repo, nil); err != nil || !slices.Equal(got, []string{".agent/tasks"}) {
		t.Fatalf("default = %v err %v", got, err)
	}
	// Relative flags → repo-relative, untouched (a monorepo's per-component trees).
	got, err := taskQueues(cfg, repo, []string{"portal/.agent/tasks", "runner/.agent/tasks"})
	if err != nil || !slices.Equal(got, []string{"portal/.agent/tasks", "runner/.agent/tasks"}) {
		t.Fatalf("relative = %v err %v", got, err)
	}
	// An absolute path inside the repo is relativized.
	abs := filepath.Join(repo, "mcp", ".agent", "tasks")
	if got, err := taskQueues(cfg, repo, []string{abs}); err != nil || !slices.Equal(got, []string{filepath.Join("mcp", ".agent", "tasks")}) {
		t.Fatalf("absolute = %v err %v", got, err)
	}
	// A path escaping the repo is rejected.
	if _, err := taskQueues(cfg, repo, []string{"../outside/tasks"}); err == nil {
		t.Error("a path escaping the repo should error")
	}
}

// A nested member's full path is a mouthful (terraform/environments/va1); the last segment is
// what anyone reaches for. It resolves only when unambiguous — silently picking one of two
// same-named members would file work in the wrong queue.
func TestFindTaskProjectChoiceShorthand(t *testing.T) {
	choices := []taskProjectChoice{
		{Name: "root", Rel: ".agent/tasks"},
		{Name: "terraform/environments/va1", Rel: "terraform/environments/va1/.agent/tasks"},
		{Name: "portal", Rel: "portal/.agent/tasks"},
	}
	for _, tc := range []struct{ in, want string }{
		{"terraform/environments/va1", "terraform/environments/va1"}, // full path
		{"va1", "terraform/environments/va1"},                        // shorthand
		{"portal", "portal"},                                         // depth-1 is its own basename
		{"root", "root"},
	} {
		got, ok := findTaskProjectChoice(choices, tc.in)
		if !ok || got.Name != tc.want {
			t.Errorf("findTaskProjectChoice(%q) = (%q, %v), want %q", tc.in, got.Name, ok, tc.want)
		}
	}

	// Two members ending in the same segment: ambiguous, so it must NOT resolve — and the error
	// has to name both, or you cannot tell why your unambiguous-looking name was rejected.
	ambiguous := []taskProjectChoice{
		{Name: "root", Rel: ".agent/tasks"},
		{Name: "apps/web", Rel: "apps/web/.agent/tasks"},
		{Name: "site/web", Rel: "site/web/.agent/tasks"},
	}
	if got, ok := findTaskProjectChoice(ambiguous, "web"); ok {
		t.Errorf("ambiguous shorthand resolved to %q, want a refusal", got.Name)
	}
	err := taskProjectAddError("web", ambiguous)
	for _, want := range []string{"matches 2 projects", "apps/web", "site/web", "use the full path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error missing %q:\n%s", want, err)
		}
	}
}
