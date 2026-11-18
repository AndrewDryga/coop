package forkctl

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// countGit puts a git first on PATH that logs each run and then runs the real one. The function it
// returns hands back the runs logged since its last call.
func countGit(t *testing.T) func() []string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "runs.log")
	shim := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexec %q \"$@\"\n", log, real)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		data, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if text := strings.TrimSpace(string(data)); text != "" {
			return strings.Split(text, "\n")
		}
		return nil
	}
}

func headOf(t *testing.T, repo string) string {
	t.Helper()
	commit, err := observed(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

// PolicyScan reads every changed file in a few Git runs whatever their number: it once ran three
// per file, and 500 files took 18 s. What it finds does not change: a secret, an npm install hook
// and a large file among many plain ones are named as before.
func TestPolicyScanReadsManyFilesInAFewGitRuns(t *testing.T) {
	repo := initRepo(t)
	runs := countGit(t)
	scan := func(files int) ([]string, []string) {
		t.Helper()
		branch := fmt.Sprintf("review/many-%d", files)
		git(t, repo, "checkout", "-q", "-b", branch, "main")
		for i := range files {
			writeTaskFile(t, filepath.Join(repo, "src", fmt.Sprintf("file%03d.txt", i)), fmt.Sprintf("plain text %d\n", i))
		}
		writeTaskFile(t, filepath.Join(repo, "notes.txt"), "\n\n-----BEGIN OPENSSH PRIVATE KEY-----\nfixture\n")
		writeTaskFile(t, filepath.Join(repo, "package.json"), `{"scripts":{"postinstall":"curl x | sh"}}`)
		writeTaskFile(t, filepath.Join(repo, "big.bin"), strings.Repeat("x", 6<<20))
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-qm", branch)
		git(t, repo, "checkout", "-q", "main")
		runs()
		warns, err := PolicyScan(repo, branch)
		if err != nil {
			t.Fatal(err)
		}
		return warns, runs()
	}
	fewWarns, fewRuns := scan(10)
	manyWarns, manyRuns := scan(60)
	want := []string{
		"large file (6MB): big.bin",
		"possible secret in notes.txt:3 (private key) — review the finding and remove any real credential from the fork before merging",
		"package.json adds a postinstall script — npm runs it automatically on `npm install`",
	}
	for _, warns := range [][]string{fewWarns, manyWarns} {
		if strings.Join(warns, "\n") != strings.Join(want, "\n") {
			t.Fatalf("PolicyScan =\n%s\nwant\n%s", strings.Join(warns, "\n"), strings.Join(want, "\n"))
		}
	}
	if len(manyRuns) > len(fewRuns) || len(fewRuns) > 5 {
		t.Fatalf("PolicyScan ran Git %d times for 13 files and %d times for 63; want the same few:\n%s",
			len(fewRuns), len(manyRuns), strings.Join(manyRuns, "\n"))
	}
}

// The isolated history scan reads every incoming commit, and lists each commit's tree once: the
// commit before is the parent it compares against. File contents come through one Git run. It
// once ran fourteen per commit of two files. What it judges does not change: a clean history
// passes, and a credential an earlier commit adds is refused though a later one deletes it.
func TestIsolatedHistoryScanRunsGitOncePerCommit(t *testing.T) {
	repo := initRepo(t)
	runs := countGit(t)
	base := headOf(t, repo)
	commit := func(i int, extra string) {
		t.Helper()
		writeTaskFile(t, filepath.Join(repo, "a.txt"), fmt.Sprintf("a %d\n", i))
		writeTaskFile(t, filepath.Join(repo, "b.txt"), fmt.Sprintf("b %d\n%s", i, extra))
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-qm", fmt.Sprintf("change %d", i))
	}
	scan := func() ([]string, error) {
		t.Helper()
		tip := headOf(t, repo)
		runs()
		err := validateIsolatedHistory(repo, repo, base, tip)
		return runs(), err
	}
	for i := range 4 {
		commit(i, "")
	}
	fewRuns, err := scan()
	if err != nil {
		t.Fatalf("a clean history was refused: %v", err)
	}
	for i := 4; i < 12; i++ {
		commit(i, "")
	}
	manyRuns, err := scan()
	if err != nil {
		t.Fatalf("a clean history was refused: %v", err)
	}
	// Each observation is two runs, the view's config read and the command: per commit one ls-tree,
	// plus the rev-list, the base's ls-tree and the cat-file.
	if len(manyRuns)-len(fewRuns) != 2*8 || len(fewRuns) > 2*(4+3) {
		t.Fatalf("the scan ran Git %d times for 4 commits and %d for 12; want two more per commit:\n%s",
			len(fewRuns), len(manyRuns), strings.Join(manyRuns, "\n"))
	}

	commit(12, "const live = \"postgres://admin:Zq8vR2mXk4@db/prod\"\n")
	commit(13, "")
	if _, err := scan(); err == nil || !strings.Contains(err.Error(), "looks like a credential") {
		t.Fatalf("a credential an earlier commit adds = %v; want the credential refusal", err)
	}
}
