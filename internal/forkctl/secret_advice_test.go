package forkctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
)

func TestForkSecretAdvice(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		name := "visible"
		if hidden {
			name = "hidden"
		}
		t.Run(name, func(t *testing.T) {
			repo := initRepo(t)
			if hidden {
				writeTaskFile(t, filepath.Join(repo, ".coopignore"), "conf.yaml\n")
				git(t, repo, "add", ".coopignore")
				git(t, repo, "commit", "-qm", "hide synthetic configuration")
			}
			ws, err := forkspace.Setup(repo, "candidate")
			if err != nil {
				t.Fatal(err)
			}
			const token = "AKIA" + "1234567890ABCDEF"
			writeTaskFile(t, filepath.Join(ws, "conf.yaml"), "aws_key: "+token+"\n")
			git(t, ws, "add", "conf.yaml")
			git(t, ws, "commit", "-qm", "synthetic scan fixture")
			before := forkspace.GitOut(repo, "rev-parse", "HEAD")
			c := &Control{cfg: &config.Config{}}
			landed, err := mergeOneForTest(t, c, repo, "", "candidate", false)
			if landed || err == nil || !strings.Contains(err.Error(), "possible secret in conf.yaml:1") {
				t.Fatalf("content finding did not refuse merge: landed=%v err=%v", landed, err)
			}
			if got := forkspace.GitOut(repo, "rev-parse", "HEAD"); got != before {
				t.Fatal("refusal advanced parent HEAD")
			}
			if _, err := os.Lstat(filepath.Join(repo, "conf.yaml")); !os.IsNotExist(err) {
				t.Fatalf("refusal wrote parent file: %v", err)
			}
			warnings := strings.Join(mustPolicyScan(t, repo, "review/candidate"), "\n")
			if !strings.Contains(warnings, "review the finding and remove any real credential from the fork before merging") {
				t.Errorf("missing useful review guidance: %s", warnings)
			}
			if strings.Contains(warnings, ".coopignore") {
				t.Error("content warning offers box hiding as a way to resolve the finding")
			}
			if strings.Contains(warnings, token) || strings.Contains(err.Error(), token) {
				t.Error("content warning leaked synthetic token")
			}
			if hidden && !strings.Contains(warnings, "secret-like file: conf.yaml") {
				t.Error("hiding must retain the independent filename concern")
			}

			writeTaskFile(t, filepath.Join(ws, "conf.yaml"), "region: example\n")
			writeTaskFile(t, filepath.Join(ws, "notes.txt"), "ordinary changed file\n")
			git(t, ws, "add", "conf.yaml", "notes.txt")
			git(t, ws, "commit", "-qm", "remove synthetic credential")
			if err := gitFetchInto(repo, ws, "candidate"); err != nil {
				t.Fatal(err)
			}
			want := ""
			if hidden {
				want = "secret-like file: conf.yaml"
			}
			if got := strings.Join(mustPolicyScan(t, repo, "review/candidate"), "\n"); got != want {
				t.Errorf("after credential removal: %q, want %q", got, want)
			}
		})
	}
}

// The merge's secret check judges only the lines the fork adds. A fake credential the project
// already had stays quiet in a file the fork edits or renames (emisar's redaction tests held up a
// merge that never touched them), while one the fork adds, even to that same file, is named at
// its own line.
func TestPolicyScanFlagsOnlySecretsTheForkAdds(t *testing.T) {
	repo := initRepo(t)
	const existing = "package mcp\n\nconst dsn = \"postgres://user:hunter2@db/app\"\n"
	writeTaskFile(t, filepath.Join(repo, "mcp", "main_test.go"), existing)
	writeTaskFile(t, filepath.Join(repo, "mcp", "old_name.go"), "package mcp\n\nconst other = \"postgres://user:hunter2@db/other\"\n")
	git(t, repo, "add", "mcp")
	git(t, repo, "commit", "-qm", "redaction fixtures")
	ws, err := forkspace.Setup(repo, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(ws, "mcp", "main_test.go"), existing+"\nfunc helper() {}\n")
	git(t, ws, "add", "mcp/main_test.go")
	git(t, ws, "mv", "mcp/old_name.go", "mcp/new_name.go")
	git(t, ws, "commit", "-qm", "edit around the fixtures")
	if err := gitFetchInto(repo, ws, "candidate"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(mustPolicyScan(t, repo, "review/candidate"), "\n"); strings.Contains(got, "possible secret") {
		t.Fatalf("the project's own fixtures held up the fork:\n%s", got)
	}

	writeTaskFile(t, filepath.Join(ws, "mcp", "main_test.go"), existing+"\nfunc helper() {}\n\nconst live = \"postgres://admin:Zq8vR2mXk4@db/prod\"\n")
	git(t, ws, "commit", "-qam", "add a credential")
	if err := gitFetchInto(repo, ws, "candidate"); err != nil {
		t.Fatal(err)
	}
	got := mustPolicyScan(t, repo, "review/candidate")
	if len(got) != 1 || !strings.Contains(got[0], "possible secret in mcp/main_test.go:7 ") {
		t.Fatalf("warnings = %q, want only the added credential at line 7", got)
	}
}
