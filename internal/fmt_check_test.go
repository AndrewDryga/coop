package internal_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// The format gate judges the Go files git lists instead of walking the tree. A recursive
// `gofmt -l .` also walked ignored task scratch, where a box hides secret-shaped paths, and the
// gate failed on "operation not permitted" before judging any file (2026-11-05).
func TestFormatGateJudgesListedGoFilesAndSkipsIgnoredScratch(t *testing.T) {
	for _, tool := range []string{"make", "gofmt"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	makefile, err := filepath.Abs(filepath.Join("..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	repo, run := gitrepo.New(t)
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const formatted, unformatted = "package x\n", "package x\nfunc  f(){}\n"
	write(".gitignore", "scratch/\n")
	write("ok.go", formatted)
	write("gone.go", formatted)
	run("add", ".")
	run("commit", "-qm", "seed")
	// Ignored scratch holding unformatted Go behind a directory nobody may open.
	write("scratch/secret/bad.go", unformatted)
	secret := filepath.Join(repo, "scratch", "secret")
	if err := os.Chmod(secret, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o755) })
	if os.Geteuid() != 0 { // root opens anything, so only an unprivileged run reproduces the walk failure
		if out, err := exec.Command("gofmt", "-l", repo).CombinedOutput(); err == nil {
			t.Fatalf("fixture no longer reproduces the failure: a recursive gofmt passed:\n%s", out)
		}
	}
	// A tracked file deleted from the work tree is not a file to judge.
	if err := os.Remove(filepath.Join(repo, "gone.go")); err != nil {
		t.Fatal(err)
	}

	fmtCheck := func() (string, error) {
		cmd := exec.Command("make", "-s", "-f", makefile, "fmt-check")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := fmtCheck(); err != nil {
		t.Fatalf("formatted sources beside ignored, unreadable scratch failed the gate: %v\n%s", err, out)
	}
	for _, tc := range []struct{ name, file string }{
		{"tracked", "ok.go"},
		{"new and not ignored", "fresh.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(tc.file, unformatted)
			t.Cleanup(func() { write(tc.file, formatted) })
			out, err := fmtCheck()
			if err == nil || !strings.Contains(out, tc.file) || !strings.Contains(out, "gofmt: files need formatting") {
				t.Errorf("an unformatted %s file passed the gate (err %v):\n%s", tc.name, err, out)
			}
		})
	}
}
