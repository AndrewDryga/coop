package cli

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

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
