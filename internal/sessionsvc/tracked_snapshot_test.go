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
