package box

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// launch computes a launch's mounts and remembers what they hid, as box.Run does.
func launch(t *testing.T, repo string) []Mount {
	t.Helper()
	mounts, err := ComputeMounts(repo, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := RememberHiddenFiles(repo, "/workspace", mounts); err != nil {
		t.Fatal(err)
	}
	return mounts
}

// A box can move a hidden file, or the folder holding it, to a path no rule names. Whatever a
// launch hid stays hidden there, from the next launch and from Coop's own tools: a file a wildcard
// path rule hid, and a folder a nested .coopignore hid.
func TestAHiddenFileStaysHiddenAfterABoxMovesIt(t *testing.T) {
	pinHome(t)
	t.Run("wildcard path rule", func(t *testing.T) {
		repo := t.TempDir()
		writeTree(t, repo, map[string]string{".coopignore": "config/*.yml\n", "config/prod.yml": "secret: 1\n"})
		if m := find(launch(t, repo), "/workspace/config/prod.yml"); m == nil || m.Kind != Decoy {
			t.Fatalf("config/prod.yml was not hidden at first: %+v", m)
		}
		if err := os.Rename(filepath.Join(repo, "config"), filepath.Join(repo, "cfg")); err != nil {
			t.Fatal(err)
		}
		if m := find(launch(t, repo), "/workspace/cfg/prod.yml"); m == nil || m.Kind != Decoy {
			t.Fatalf("the moved file was shown to the next launch: %+v", m)
		}
		if hidden, err := NewShadowDecider(repo)("cfg/prod.yml"); err != nil || !hidden {
			t.Fatalf("Coop's host tools read the moved file as visible: %v, %v", hidden, err)
		}
		// Still hidden on every launch after, not only the first one after the move.
		if m := find(launch(t, repo), "/workspace/cfg/prod.yml"); m == nil || m.Kind != Decoy {
			t.Fatalf("the moved file came back on a later launch: %+v", m)
		}
	})
	t.Run("nested .coopignore", func(t *testing.T) {
		repo := t.TempDir()
		writeTree(t, repo, map[string]string{"config/.coopignore": "secrets/\n", "config/secrets/token": "t0k3n\n", "other/readme": "x\n"})
		if m := find(launch(t, repo), "/workspace/config/secrets"); m == nil || m.Kind != DirDecoy {
			t.Fatalf("config/secrets was not hidden at first: %+v", m)
		}
		if err := os.Rename(filepath.Join(repo, "config", "secrets"), filepath.Join(repo, "other", "secrets")); err != nil {
			t.Fatal(err)
		}
		if m := find(launch(t, repo), "/workspace/other/secrets"); m == nil || m.Kind != DirDecoy {
			t.Fatalf("the moved folder was shown to the next launch: %+v", m)
		}
		if hidden, err := NewShadowDecider(repo)("other/secrets/token"); err != nil || !hidden {
			t.Fatalf("Coop's host tools read a file in the moved folder as visible: %v, %v", hidden, err)
		}
	})
}

// Remembering never overrides the person's rules where the file still is: back at its path, a file
// whose rule was taken out of .coopignore is shown, and forgotten.
func TestAFileStillAtItsPathFollowsTheCurrentRules(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{".coopignore": "config/*.yml\n", "config/prod.yml": "secret: 1\n"})
	launch(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".coopignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if m := find(launch(t, repo), "/workspace/config/prod.yml"); m != nil {
		t.Fatalf("a file whose rule was removed stayed hidden: %+v", m)
	}
	if remembered := loadHiddenFiles(repo); len(remembered) != 0 {
		t.Fatalf("a shown file is still remembered: %v", remembered)
	}
}
