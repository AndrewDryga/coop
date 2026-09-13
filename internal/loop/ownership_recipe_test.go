package loop

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// These execute the prompt's Git recipes, not a model's interpretation of them.
// Native agent compliance is qualified separately.
func TestLoopOwnershipCommitRecipe(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	for _, name := range []string{"owned.txt", "staged.txt", "unstaged.txt"} {
		writeTaskFile(t, filepath.Join(repo, name), "base\n")
	}
	writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/tasks/\n")
	git("add", "--", ".gitignore", "owned.txt", "staged.txt", "unstaged.txt")
	git("commit", "-m", "base")
	writeTaskFile(t, filepath.Join(repo, "staged.txt"), "foreign staged\n")
	git("add", "--", "staged.txt")
	contents := map[string]string{
		"staged.txt": "foreign staged plus unstaged\n", "unstaged.txt": "foreign unstaged\n",
		"untracked.txt": "foreign untracked\n", ".agent/tasks/current/task.md": "ignored task state\n",
	}
	for name, body := range contents {
		writeTaskFile(t, filepath.Join(repo, name), body)
	}
	foreignIndex := gitOut(repo, "ls-files", "--stage", "--", "staged.txt", "unstaged.txt", "untracked.txt", ".agent/tasks")
	writeTaskFile(t, filepath.Join(repo, "owned.txt"), "task change\n")
	git("add", "--", "owned.txt")
	git("diff", "--cached")
	git("commit", "--only", "-m", "Task change\n\nCoop-Task: current", "--", "owned.txt")
	if got := gitOut(repo, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"); got != "owned.txt" {
		t.Fatalf("commit included unowned paths: %q", got)
	}
	if got := gitOut(repo, "ls-files", "--stage", "--", "staged.txt", "unstaged.txt", "untracked.txt", ".agent/tasks"); got != foreignIndex {
		t.Fatalf("foreign index changed: before %q, after %q", foreignIndex, got)
	}
	for name, want := range contents {
		got, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil || string(got) != want {
			t.Fatalf("foreign file %s changed: %q, %v", name, got, err)
		}
	}
	git("check-ignore", "--quiet", ".agent/tasks/current/task.md")
}

func TestLoopOwnershipPreflightBeforeRestoration(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "unstaged", true: "staged"}[staged], func(t *testing.T) {
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, "source.txt"), "base\n")
			git("add", "source.txt")
			git("commit", "-m", "base")
			writeTaskFile(t, filepath.Join(repo, "source.txt"), "foreign edits\n")
			if staged {
				git("add", "source.txt")
			}
			before := gitOut(repo, "ls-files", "--stage")
			// Refuse ambiguous source before installing any cleanup. The marker
			// models an unsafe restore hook without ever running a destructive one.
			cmd := exec.Command("sh", "-c", `git diff --quiet -- source.txt && git diff --cached --quiet -- source.txt || exit 17
trap 'touch restore-ran' EXIT
exit 0`)
			cmd.Dir = repo
			out, err := cmd.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
				t.Fatalf("preflight did not refuse: %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(repo, "restore-ran")); !os.IsNotExist(err) {
				t.Fatalf("refusal armed restoration: %v", err)
			}
			if got := gitOut(repo, "ls-files", "--stage"); got != before {
				t.Fatalf("refusal changed index: %q -> %q", before, got)
			}
			got, err := os.ReadFile(filepath.Join(repo, "source.txt"))
			if err != nil || string(got) != "foreign edits\n" {
				t.Fatalf("refusal changed source: %q, %v", got, err)
			}
		})
	}
}
