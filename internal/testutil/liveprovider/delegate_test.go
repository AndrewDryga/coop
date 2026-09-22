package liveprovider

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestVerifyDelegateRepository(t *testing.T) {
	for _, change := range []string{"only output", "missing", "wrong content", "extra", "ignored", "staged", "commit", "git config", "symlink", "hardlink"} {
		t.Run(change, func(t *testing.T) {
			layout, err := procharness.NewLayout(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := InitRepository(layout); err != nil {
				t.Fatal(err)
			}
			before, err := SnapshotRepository(layout)
			if err != nil {
				t.Fatal(err)
			}
			const name, content = "delegate-result.txt", "EXPECTED_MARKER\n"
			path := filepath.Join(layout.Repo, name)
			if change != "missing" {
				writeSource(t, path, content, 0o600)
			}
			switch change {
			case "wrong content":
				writeSource(t, path, "near miss\n", 0o600)
			case "extra":
				writeSource(t, filepath.Join(layout.Repo, "extra"), "unexpected", 0o600)
			case "ignored":
				writeSource(t, filepath.Join(layout.Repo, ".agent", "extra"), "unexpected", 0o600)
			case "staged", "commit":
				if _, err := runGit(layout, "add", name); err != nil {
					t.Fatal(err)
				}
				if change == "commit" {
					if _, err := runGit(layout, "commit", "-qm", "unexpected"); err != nil {
						t.Fatal(err)
					}
				}
			case "git config":
				writeSource(t, filepath.Join(layout.Repo, ".git", "config"), "invalid config\n", 0o600)
			case "symlink":
				if err := os.Rename(path, filepath.Join(layout.State, "real")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(layout.State, "real"), path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(layout.State, "alias")); err != nil {
					t.Fatal(err)
				}
			}
			err = VerifyDelegateRepository(layout, before, name, content)
			if (err == nil) != (change == "only output") {
				t.Fatalf("delegate verification: %v", err)
			}
			_, statErr := os.Lstat(path)
			if change == "only output" {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("verified disposable output was not removed")
				}
			} else if change != "missing" && statErr != nil {
				t.Fatal("failed verification removed evidence")
			}
		})
	}
}
