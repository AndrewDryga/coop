package loop

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// A loop started outside Git hears that it is outside Git, and how to fix that, rather than being
// told to commit into a repository that does not exist.
func TestNoHeadCauseNamesAFolderOutsideGit(t *testing.T) {
	if got := noHeadCause(t.TempDir()); !strings.HasPrefix(got, "This folder is not a Git repository.\nRun git init") {
		t.Errorf("outside Git: %q", got)
	}
	repo, _ := gitrepo.New(t)
	if got := noHeadCause(repo); !strings.HasPrefix(got, "This repository has no readable Git commit yet.\n") {
		t.Errorf("a repository with no commit: %q", got)
	}
}
