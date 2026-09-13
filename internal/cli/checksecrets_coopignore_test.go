package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/secretscan"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestCheckSecretsCoopignoreCommitCandidates(t *testing.T) {
	pinGitConfig(t)
	for _, scope := range []string{"root", "nested", "root directory", "nested directory"} {
		for _, state := range []string{"untracked", "tracked", "tracked then ignored"} {
			t.Run(scope+"/"+state, func(t *testing.T) {
				repo, git := gitrepo.New(t)
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
				const candidate = "config/private/client.go"
				write(candidate, openAIKeyFile())
				if state != "untracked" {
					git("add", "--", candidate)
					git("commit", "-qm", "synthetic scan fixture")
				}
				ignore := "local-only/\nconfig/private/ignored.txt\n"
				if state == "tracked then ignored" {
					ignore += candidate + "\n"
				}
				write(".gitignore", ignore)
				write("local-only/sub/notes.txt", openAIKeyFile())
				write("config/private/ignored.txt", openAIKeyFile())
				rootRules := "local-only/\nconfig/private/ignored.txt\n"
				shadowTarget, shadowKind := candidate, box.Decoy
				switch scope {
				case "root":
					rootRules += candidate + "\n"
				case "nested":
					write("config/.coopignore", "private/client.go\n")
				case "root directory":
					rootRules += "config/private/\n"
					shadowTarget, shadowKind = "config/private", box.DirDecoy
				case "nested directory":
					write("config/.coopignore", "private/\n")
					shadowTarget, shadowKind = "config/private", box.DirDecoy
				}
				write(".coopignore", rootRules)
				mounts, err := box.ComputeMounts(repo, "/workspace")
				if err != nil {
					t.Fatal(err)
				}
				hidden := false
				for _, mount := range mounts {
					if mount.Target == "/workspace/"+shadowTarget && mount.Kind == shadowKind && mount.RO {
						hidden = true
					}
				}
				if !hidden {
					t.Fatal("candidate is not protected by the box mount plan")
				}
				if count := unscannedIgnoredCount(repo); count != 0 {
					t.Errorf("hidden ignored descendants counted as visible: %d", count)
				}
				for _, includeIgnored := range []bool{false, true} {
					scan, err := scanVisibleTree(repo, includeIgnored)
					if err != nil {
						t.Fatal(err)
					}
					if len(scan.findings) != 1 || scan.findings[0].Path != candidate || !scan.findings[0].shadowed {
						t.Fatalf("includeIgnored=%v: want only the hidden commit candidate, got %s", includeIgnored, joinScan(scan))
					}
				}
			})
		}
	}
}

func TestCheckSecretsCoopignoreExactExceptions(t *testing.T) {
	pinGitConfig(t)
	const candidate = "config/client.go"
	original := openAIKeyFile()
	repo := scanRepo{git: true, files: map[string]string{
		".coopignore": candidate + "\n", candidate: original,
	}}.build(t)
	out, code := runCheckSecrets(t, repo, nil)
	if code != 1 {
		t.Fatalf("hidden commit candidate must fail the scan: exit=%d output=%s", code, out)
	}
	id := fingerprintIn(t, out)
	if err := os.WriteFile(filepath.Join(repo, secretscan.ExceptionsFile), []byte(id+" # Public synthetic fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, body    string
		code          int
		ignoredNotice bool
	}{
		{"reviewed original", original, 0, true},
		{"inserted line", "// another line\n" + original, 0, true},
		{"changed value", strings.Replace(original, "N7qFvZm2", "P8rGwYn3", 1), 1, false},
		{"independent finding", original + "\n" + testPrivateKey, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(repo, candidate), []byte(test.body), 0o644); err != nil {
				t.Fatal(err)
			}
			out, code := runCheckSecrets(t, repo, nil)
			if code != test.code {
				t.Fatalf("scan exit=%d, want %d: %s", code, test.code, out)
			}
			if got := strings.Contains(out, "1 finding ignored by .coopsecretsignore"); got != test.ignoredNotice {
				t.Fatalf("reviewed finding notice=%v, want %v: %s", got, test.ignoredNotice, out)
			}
			if code == 1 && (strings.Contains(out, id) || !strings.Contains(out, "1 possible secret found")) {
				t.Fatal("scan must report only the changed or independent finding")
			}
			if strings.Contains(out, "sk-proj-") || strings.Contains(out, "b3BlbnNzaC1r") {
				t.Fatal("scan output exposed synthetic credential material")
			}
		})
	}
}
