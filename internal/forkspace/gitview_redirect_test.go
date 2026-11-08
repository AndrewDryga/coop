package forkspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A box can write every file in .git. Each pointer below would make host git read another
// repository the user can open, so views and the ref commands refuse it; the layouts git itself
// makes for worktrees and submodules keep working, and so do shared clones (alternates), which hand
// over an object only to one who knows its name.
func TestHostGitRefusesPointersIntoAnotherRepository(t *testing.T) {
	other, _ := viewTestRepo(t)
	otherGit := filepath.Join(other, ".git")

	write := func(t *testing.T, path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := func(t *testing.T, target, path string) {
		t.Helper()
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, repo string)
		fix   string
	}{
		{"commondir in a real .git", func(t *testing.T, repo string) {
			write(t, filepath.Join(repo, ".git", "commondir"), otherGit+"\n")
		}, "delete it"},
		{".git file naming another repository's git dir", func(t *testing.T, repo string) {
			if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(repo, ".git"), "gitdir: "+otherGit+"\n")
		}, "Restore this folder's own .git"},
		{".git linked to another repository's .git", func(t *testing.T, repo string) {
			link(t, otherGit, filepath.Join(repo, ".git"))
		}, "Restore this folder's own .git"},
		{"separate git dir", func(t *testing.T, repo string) {
			moved := filepath.Join(t.TempDir(), "store")
			if err := os.Rename(filepath.Join(repo, ".git"), moved); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(repo, ".git"), "gitdir: "+moved+"\n")
		}, "move it back"},
		{"forged submodule git dir linked to another repository", func(t *testing.T, repo string) {
			sub := filepath.Join(repo, "sub")
			modules := filepath.Join(repo, ".git", "modules")
			if err := os.MkdirAll(modules, 0o700); err != nil {
				t.Fatal(err)
			}
			link(t, otherGit, filepath.Join(modules, "sub"))
			write(t, filepath.Join(sub, ".git"), "gitdir: ../.git/modules/sub\n")
			for _, refused := range []error{
				func() error { _, err := GitCommand(t.Context(), sub, "status"); return err }(),
				GitRefCommand(t.Context(), sub, "update-ref", "refs/heads/x", "HEAD").Run(),
			} {
				if refused == nil || !strings.Contains(refused.Error(), "outside this repository") || strings.Contains(refused.Error(), "mv ") {
					t.Fatalf("the forged submodule = %v; want it refused without advice to move a Git folder", refused)
				}
			}
		}, ""},
		{"linked pack file", func(t *testing.T, repo string) {
			link(t, filepath.Join(otherGit, "objects", "pack", "pack-x.pack"), filepath.Join(repo, ".git", "objects", "pack", "pack-x.pack"))
		}, "Replace the link"},
		{"linked objects", func(t *testing.T, repo string) {
			link(t, filepath.Join(otherGit, "objects"), filepath.Join(repo, ".git", "objects"))
		}, "Replace the link"},
		{"linked refs", func(t *testing.T, repo string) {
			link(t, filepath.Join(otherGit, "refs"), filepath.Join(repo, ".git", "refs"))
		}, "Replace the link"},
		{"linked pack folder", func(t *testing.T, repo string) {
			link(t, filepath.Join(otherGit, "objects", "pack"), filepath.Join(repo, ".git", "objects", "pack"))
		}, "Replace the link"},
		{"linked loose-object folder", func(t *testing.T, repo string) {
			link(t, filepath.Join(otherGit, "objects", "ab"), filepath.Join(repo, ".git", "objects", "ab"))
		}, "Replace the link"},
		{"linked branch file", func(t *testing.T, repo string) {
			link(t, filepath.Join(otherGit, "refs", "heads", "main"), filepath.Join(repo, ".git", "refs", "heads", "borrowed"))
		}, "Replace the link"},
		{"linked packed-refs", func(t *testing.T, repo string) {
			link(t, filepath.Join(otherGit, "packed-refs"), filepath.Join(repo, ".git", "packed-refs"))
		}, "Replace the link"},
		{"HEAD linked outside refs", func(t *testing.T, repo string) {
			link(t, filepath.Join(otherGit, "HEAD"), filepath.Join(repo, ".git", "HEAD"))
		}, "Replace the link"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := viewTestRepo(t)
			tc.plant(t, repo)
			if tc.fix == "" {
				return
			}
			_, err := GitCommand(t.Context(), repo, "status")
			if err == nil || !strings.Contains(err.Error(), "outside this repository") || !strings.Contains(err.Error(), tc.fix) {
				t.Fatalf("view = %v; want a refusal that says %q", err, tc.fix)
			}
			if err := GitRefCommand(t.Context(), repo, "update-ref", "refs/heads/x", "HEAD").Run(); err == nil || !strings.Contains(err.Error(), "outside this repository") {
				t.Fatalf("ref command = %v; want it refused", err)
			}
		})
	}
}

func TestHostGitFollowsTheLayoutsGitMakes(t *testing.T) {
	repo, run := viewTestRepo(t)
	sub, runSub := viewTestRepo(t)
	runSub("tag", "v1")
	run("-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	run("commit", "-qm", "add submodule")
	nested := filepath.Join(repo, "sub")
	if out, err := viewRun(t, nested, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("a submodule's view = %q, %v", out, err)
	}

	shared := filepath.Join(t.TempDir(), "shared")
	run("clone", "-q", "--shared", repo, shared)
	if out, err := viewRun(t, shared, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("a shared clone's view = %q, %v", out, err)
	}

	worktree := filepath.Join(t.TempDir(), "wt")
	run("worktree", "add", "-q", worktree, "-b", "wt")
	if out, err := viewRun(t, worktree, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("a linked worktree's view = %q, %v", out, err)
	}
	// A linked worktree keeps its submodules' git dirs under its own git dir.
	run("-C", worktree, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	if out, err := viewRun(t, filepath.Join(worktree, "sub"), "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("a submodule in a linked worktree = %q, %v", out, err)
	}

	// A nested submodule, and a submodule whose sparse checkout moved core.worktree into
	// config.worktree.
	runSub("-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "inner")
	runSub("commit", "-qm", "add inner")
	run("-C", nested, "pull", "-q", "origin", "HEAD")
	run("-C", nested, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	if out, err := viewRun(t, filepath.Join(nested, "inner"), "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("a nested submodule's view = %q, %v", out, err)
	}
	run("-C", nested, "sparse-checkout", "init")
	if _, err := os.Stat(filepath.Join(repo, ".git", "modules", "sub", "config.worktree")); err != nil {
		t.Fatalf("sparse-checkout did not move core.worktree: %v", err)
	}
	if _, err := viewRun(t, nested, "status", "--porcelain"); err != nil {
		t.Fatalf("a sparse submodule's view = %v", err)
	}

	// A box working in one submodule cannot reach a sibling's git dir: its backlink names the
	// sibling.
	run("-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sibling")
	if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: ../.git/modules/sibling\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gitDirsOf(nested); err == nil || !strings.Contains(err.Error(), "belongs to the submodule at") {
		t.Fatalf("a submodule pointing at its sibling's git dir = %v; want it refused", err)
	}
}
