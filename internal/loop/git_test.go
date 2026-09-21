package loop

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// A worker can edit .git/config and .gitattributes. The host must still be able to inspect
// its changes without executing a repository-defined clean filter outside the box.
func TestLoopGitNeverExecutesRepositoryDrivers(t *testing.T) {
	for _, operation := range []string{"status", "review snapshot", "NUL paths"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
			t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
			t.Cleanup(forkspace.CloseGitViews)
			repo, git := gitrepo.New(t)
			name := " tracked file \n.txt "
			writeTaskFile(t, filepath.Join(repo, name), "before\n")
			git("add", "--", name)
			git("commit", "-qm", "base")
			head := gitOut(repo, "rev-parse", "HEAD")
			if head == "" {
				t.Fatal("fixture HEAD is missing")
			}
			marker := filepath.Join(t.TempDir(), "host-execution")
			t.Setenv("COOP_TEST_GIT_MARKER", marker)
			driver := filepath.Join(repo, "driver.sh")
			writeTaskFile(t, driver, "#!/bin/sh\nprintf invoked > \"$COOP_TEST_GIT_MARKER\"\ncat\n")
			if err := os.Chmod(driver, 0o700); err != nil {
				t.Fatal(err)
			}
			writeTaskFile(t, filepath.Join(repo, ".gitattributes"), "* filter=audit\n")
			git("config", "filter.audit.clean", driver)
			writeTaskFile(t, filepath.Join(repo, name), "after!\n")

			switch operation {
			case "status":
				out, err := gitOutErr(repo, "status", "--porcelain=v1", "--untracked-files=no")
				if err != nil || !strings.Contains(out, "tracked file") {
					t.Fatalf("status did not report the tracked edit: %q, %v", out, err)
				}
			case "review snapshot":
				snapshot, err := snapshotReviewSource(repo)
				if err != nil || snapshot.head != head || !strings.Contains(snapshot.trackedState, "tracked file") {
					t.Fatalf("review snapshot lost source state: %+v, %v", snapshot, err)
				}
			case "NUL paths":
				if paths := gitNULPaths(repo, "diff", "--name-only", "-z"); !slices.Equal(paths, []string{name}) {
					t.Fatalf("changed paths = %q, want exact path %q", paths, name)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("loop %s executed a repository driver on the host: %v", operation, err)
			}

			// Prove this is a working hostile fixture, not an inert filter configuration.
			writeTaskFile(t, filepath.Join(repo, name), "again!\n")
			args := append(append([]string{"-C", repo}, forkspace.GitHardening...), "status", "--porcelain")
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("positive-control status: %v: %s", err, out)
			}
			if content, err := os.ReadFile(marker); err != nil || string(content) != "invoked" {
				t.Fatalf("positive control did not execute the filter: %q, %v", content, err)
			}
		})
	}
}
