package sessionsvc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestTrackedSnapshotRetainsRealEditsWithoutFiltersOrIndexMutation(t *testing.T) {
	for _, scenario := range []string{"clean", "unrelated", "dirty-lfs", "staged-add-delete", "hidden-split-index", "literal-pathspec"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
			t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
			repository, git := gitrepo.New(t)
			payload := bytes.Repeat([]byte("large\x00payload\n"), 200_000)
			writeSessionLFSSource(t, repository, payload)
			sessionWorkspaceWrite(t, filepath.Join(repository, "ordinary"), "original ordinary\n")
			sessionWorkspaceWrite(t, filepath.Join(repository, ":(glob)*"), "literal original\n")
			git("add", ".")
			git("commit", "-qm", "source")
			head := gitOut(repository, "rev-parse", "HEAD")
			if err := forkspace.HydrateLFS(context.Background(), repository, head); err != nil {
				t.Fatal(err)
			}
			want := ""
			switch scenario {
			case "unrelated", "hidden-split-index":
				if scenario == "hidden-split-index" {
					git("update-index", "--split-index")
					git("update-index", "--skip-worktree", "ordinary")
				}
				sessionWorkspaceWrite(t, filepath.Join(repository, "ordinary"), "genuine ordinary edit\n")
				want = "+genuine ordinary edit"
			case "dirty-lfs":
				sessionWorkspaceWrite(t, filepath.Join(repository, "asset.bin"), "genuine LFS edit\n")
				want = "+genuine LFS edit"
			case "staged-add-delete":
				git("rm", "ordinary")
				sessionWorkspaceWrite(t, filepath.Join(repository, "new"), "staged addition\n")
				git("add", "new")
				want = "+staged addition"
			case "literal-pathspec":
				sessionWorkspaceWrite(t, filepath.Join(repository, ":(glob)*"), "literal edit\n")
				want = "+literal edit"
			}
			marker := filepath.Join(t.TempDir(), "filter-ran")
			git("config", "filter.lfs.clean", "touch '"+marker+"'; exit 1")
			index, err := os.ReadFile(filepath.Join(repository, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			var patch bytes.Buffer
			if err := streamSessionTrackedPatch(context.Background(), repository, head, &patch); err != nil {
				t.Fatal(err)
			}
			if scenario == "clean" && patch.Len() != 0 || want != "" && !strings.Contains(patch.String(), want) {
				t.Fatalf("snapshot lost or invented edits: %q", patch.String())
			}
			if scenario != "dirty-lfs" && strings.Contains(patch.String(), "asset.bin") {
				t.Fatal("snapshot included an unchanged hydrated LFS asset")
			}
			if scenario == "staged-add-delete" && !strings.Contains(patch.String(), "-original ordinary") {
				t.Fatal("snapshot lost the staged deletion")
			}
			if after, err := os.ReadFile(filepath.Join(repository, ".git", "index")); err != nil || !bytes.Equal(after, index) {
				t.Fatal("snapshot rewrote the model's index")
			}
			if pathExists(marker) {
				t.Fatal("snapshot executed the model's filter")
			}
		})
	}
}

func TestTrackedSnapshotStagesLiteralChangesWithoutTouchingAgentIndex(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	for _, path := range []string{"deleted", "executable", "-literal\nname", "staged"} {
		sessionWorkspaceWrite(t, filepath.Join(repo, path), "base\n")
	}
	if err := os.Symlink("old-target", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "snapshot fixture")
	sessionWorkspaceWrite(t, filepath.Join(repo, "staged"), "staged only\n")
	git("add", "staged")
	if err := os.Remove(filepath.Join(repo, "deleted")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repo, "executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("new-target", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(repo, "-literal\nname"), "changed literal\n")
	sessionWorkspaceWrite(t, filepath.Join(repo, "untracked"), "not part of tracked snapshot\n")
	before, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	err = withSessionTrackedTree(t.Context(), repo, func(tree string, env []string) error {
		listing, _, err := runSessionPrivateGitContext(context.Background(), repo, 4096, env, "ls-tree", "-z", tree)
		if err != nil {
			return err
		}
		if strings.Contains(string(listing), "deleted") || strings.Contains(string(listing), "untracked") || !strings.Contains(string(listing), "100755 blob") {
			t.Fatalf("wrong tracked tree: %q", listing)
		}
		for path, expected := range map[string]string{"staged": "staged only\n", "-literal\nname": "changed literal\n", "link": "new-target"} {
			body, _, err := runSessionPrivateGitContext(context.Background(), repo, 4096, env, "show", tree+":"+path)
			if err != nil || string(body) != expected {
				t.Fatalf("snapshot %q = %q, %v", path, body, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("snapshot changed agent index: %v", err)
	}
}
