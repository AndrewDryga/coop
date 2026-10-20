package gitrepo

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

type seedFile struct {
	name string
	mode fs.FileMode
	data []byte
}

// Only immutable bytes survive setup; no test borrows another test's repository or objects.
var committedSeed = sync.OnceValues(func() ([]seedFile, error) {
	root, err := os.MkdirTemp("", "coop-git-seed-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(root, "noglobal"), "GIT_CONFIG_SYSTEM="+filepath.Join(root, "nosystem"))
	env = append(env, noMaintenance...)
	for _, args := range [][]string{
		{"init", "-q"}, {"symbolic-ref", "HEAD", "refs/heads/main"},
		{"config", "user.email", "t@t"}, {"config", "user.name", "T"},
		{"commit", "-q", "--allow-empty", "-m", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = root, env
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git seed %v: %w\n%s", args, err, out)
		}
	}
	var files []seedFile
	err = filepath.WalkDir(filepath.Join(root, ".git"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file := seedFile{name: name, mode: info.Mode()}
		if !entry.IsDir() {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("non-regular seed file %s", path)
			}
			file.data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		files = append(files, file)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
})

// NewCommitted returns a private copy of a main repository with one empty base commit.
// Use New when initialization or initial commits are themselves part of the test contract.
func NewCommitted(t *testing.T) (string, func(...string)) {
	t.Helper()
	files, err := committedSeed()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	for _, file := range files {
		path := filepath.Join(repo, file.name)
		if file.mode.IsDir() {
			err = os.Mkdir(path, file.mode.Perm())
		} else {
			err = os.WriteFile(path, file.data, file.mode.Perm())
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "noglobal"), "GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "nosystem"))
	env = append(env, noMaintenance...)
	return repo, func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
