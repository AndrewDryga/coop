package cli

import (
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func hasCand(cands []string, want string) bool {
	for _, c := range cands {
		if c == want {
			return true
		}
	}
	return false
}

// completionCandidates mirrors the dispatch: commands + agents at the top, then per-family verbs.
func TestCompletionCandidates(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}}

	top := a.completionCandidates(nil)
	for _, w := range []string{"fork", "tasks", "loop", "claude", "completion"} {
		if !hasCand(top, w) {
			t.Errorf("top-level completion missing %q", w)
		}
	}
	if hasCand(top, "clone") || hasCand(top, "pool") {
		t.Error("retired aliases (clone/pool) must not be completed")
	}

	for _, w := range []string{"ls", "rm", "merge"} {
		if !hasCand(a.completionCandidates([]string{"fork"}), w) {
			t.Errorf("fork completion missing verb %q", w)
		}
	}
	if tk := a.completionCandidates([]string{"tasks"}); !hasCand(tk, "claim") || !hasCand(tk, "watch") {
		t.Errorf("tasks completion missing verbs: %v", tk)
	}
	if fl := a.completionCandidates([]string{"fleet"}); !hasCand(fl, "up") || !hasCand(fl, "prune") {
		t.Errorf("fleet completion missing verbs: %v", fl)
	}
	if !hasCand(a.completionCandidates([]string{"login"}), "claude") {
		t.Error("login completion should offer agents")
	}
	if c := a.completionCandidates([]string{"completion"}); !hasCand(c, "bash") || !hasCand(c, "zsh") {
		t.Errorf("completion completion missing shells: %v", c)
	}
}

// `coop tasks claim <TAB>` offers the queue's task ids (a local read).
func TestCompletionTaskIDs(t *testing.T) {
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-wire-auth", "task.md"), "# x\n")
	a := &app{cfg: &config.Config{RepoOverride: repo, TasksFiles: []string{tasksRoot}}}
	if ids := a.completionCandidates([]string{"tasks", "claim"}); !hasCand(ids, "2026-01-01-wire-auth") {
		t.Errorf("tasks claim completion should offer task ids, got %v", ids)
	}
	// a non-id verb does not offer ids.
	if ids := a.completionCandidates([]string{"tasks", "lint"}); hasCand(ids, "2026-01-01-wire-auth") {
		t.Error("tasks lint takes no id — should not offer task ids")
	}
}
