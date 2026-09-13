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
			before := gitOut(repo, "rev-parse", "HEAD")
			c := &Control{cfg: &config.Config{}}
			landed, err := mergeOneForTest(t, c, repo, "", "candidate", false)
			if landed || err == nil || !strings.Contains(err.Error(), "possible secret in conf.yaml:1") {
				t.Fatalf("content finding did not refuse merge: landed=%v err=%v", landed, err)
			}
			if got := gitOut(repo, "rev-parse", "HEAD"); got != before {
				t.Fatal("refusal advanced parent HEAD")
			}
			if _, err := os.Lstat(filepath.Join(repo, "conf.yaml")); !os.IsNotExist(err) {
				t.Fatalf("refusal wrote parent file: %v", err)
			}
			warnings := strings.Join(PolicyScan(repo, "review/candidate"), "\n")
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
			if got := strings.Join(PolicyScan(repo, "review/candidate"), "\n"); got != want {
				t.Errorf("after credential removal: %q, want %q", got, want)
			}
		})
	}
}
