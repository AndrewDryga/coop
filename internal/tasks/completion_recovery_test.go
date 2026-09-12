package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestUncommittedCompletionCanRetry(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	for _, scenario := range []string{"clean", "untracked", "staged", "unstaged", "advanced", "bound", "malformed", "graft", "shallow", "unreadable"} {
		t.Run(scenario, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, "source"), "original\n")
			git("add", "source")
			message := "base"
			if scenario == "bound" || scenario == "graft" || scenario == "shallow" {
				message += "\n\nCoop-Task: task"
			} else if scenario == "malformed" {
				message += "\n\nCoop-Task: task extra"
			}
			git("commit", "-m", message)
			if scenario == "graft" || scenario == "shallow" {
				git("commit", "--allow-empty", "-m", "descendant")
			}
			base := gitOut(repo, "rev-parse", "HEAD")
			switch scenario {
			case "untracked", "staged":
				writeTaskFile(t, filepath.Join(repo, "unrelated"), "preserve me\n")
				if scenario == "staged" {
					git("add", "unrelated")
				}
			case "unstaged":
				writeTaskFile(t, filepath.Join(repo, "source"), "modified\n")
			case "advanced":
				git("commit", "--allow-empty", "-m", "unbound work")
			case "graft":
				writeTaskFile(t, filepath.Join(repo, ".git", "info", "grafts"), base+"\n")
			case "shallow":
				writeTaskFile(t, filepath.Join(repo, ".git", "shallow"), base+"\n")
			case "unreadable":
				base = "not-a-commit"
			}
			head := gitOut(repo, "rev-parse", "HEAD")
			if scenario == "unreadable" {
				head = base
			}
			if got := UncommittedCompletionCanRetry(repo, base, head, "task"); got != (scenario == "clean") {
				t.Fatalf("can retry %s = %v", scenario, got)
			}
		})
	}
}

func TestParkUncommittedCompletion(t *testing.T) {
	for _, obstruct := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserves evidence", true: "refuses unsafe metadata"}[obstruct], func(t *testing.T) {
			root := t.TempDir()
			id := "task"
			dir := filepath.Join(root, StateInProgress, id)
			writeTaskFile(t, filepath.Join(dir, "task.md"), "# Decision task\n")
			writeTaskFile(t, filepath.Join(dir, "log.md"), "# Log\nEvidence to preserve\n")
			writeTaskFile(t, filepath.Join(dir, "state.md"), "# State\n")
			previous := "# Old decision\n\n**Resolution:** old answer\n"
			writeTaskFile(t, filepath.Join(dir, "decision.md"), previous)
			if obstruct {
				if err := os.Remove(filepath.Join(dir, "log.md")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("decision.md", filepath.Join(dir, "log.md")); err != nil {
					t.Fatal(err)
				}
			}
			item := mustReadTaskTree(t, root)[0]
			err := ParkUncommittedCompletion(QueuedTask{Root: root, Item: item})
			if obstruct {
				if err == nil || !pathExists(dir) || readFileString(filepath.Join(dir, "decision.md")) != previous {
					t.Fatalf("unsafe metadata park = %v; task/evidence changed", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			blocked := filepath.Join(root, StateBlocked, id)
			if pathExists(dir) || !pathExists(blocked) || mustDecisionResolved(t, filepath.Join(blocked, "decision.md")) {
				t.Fatal("task not parked with an unanswered decision")
			}
			log := readFileString(filepath.Join(blocked, "log.md"))
			if !strings.Contains(log, "Evidence to preserve") || !strings.Contains(log, previous) || !strings.Contains(log, "no completion was accepted") {
				t.Fatalf("park lost evidence: %s", log)
			}
		})
	}
}
