package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/secretscan"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// A .coopignore coop cannot read stops check-secrets as it stops a launch: the scan cannot tell
// which files the box hides, so a clean result would be false. The refusal names that .coopignore,
// says why and how to fix it, for one in the project, one in a folder above it, and one in a
// Git-ignored folder, which only the count of files the default scan skips reaches.
func TestCheckSecretsRefusesAnUnreadableCoopignore(t *testing.T) {
	// Pinned here, not through pinGitConfig: this function runs git init itself.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	kinds := []struct {
		name, reason string
		plant        func(t *testing.T, path string)
	}{
		{"link", "is a link", func(t *testing.T, path string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "rules"), path); err != nil {
				t.Fatal(err)
			}
		}},
		{"folder", "is a folder", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", "is not a regular file", func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized", "is larger than 1 MiB", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("# padding\n", 1<<20/10+1)), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	project := func(t *testing.T) (outer, repo string) {
		t.Helper()
		home, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home) // the folders above the project count up to the home folder
		outer = filepath.Join(home, "work")
		repo = filepath.Join(outer, "app")
		for rel, body := range map[string]string{"config/app.go": "package config\n", ".gitignore": "local/\n", "local/notes.txt": "notes\n"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		return outer, repo
	}
	refused := func(t *testing.T, repo, file, reason string) {
		t.Helper()
		out, code := runCheckSecrets(t, repo, nil)
		if code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, out)
		}
		for _, want := range []string{
			"Could not read " + file,
			"Coop cannot tell what " + file + " hides, because it " + reason + ".",
			"Remove " + file + " if you did not create it; otherwise make it a readable regular file of at most 1 MiB. Then run coop check-secrets again.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "No possible secrets found") || strings.Contains(out, "not checked") {
			t.Errorf("the refusal came with a scan result:\n%s", out)
		}
	}
	for _, kind := range kinds {
		for _, place := range []string{"project", "folder above"} {
			t.Run(kind.name+" in the "+place, func(t *testing.T) {
				outer, repo := project(t)
				policy, file := filepath.Join(repo, ".coopignore"), ".coopignore"
				if place == "folder above" {
					policy = filepath.Join(outer, ".coopignore")
					file = policy
				}
				kind.plant(t, policy)
				refused(t, repo, file, kind.reason)
			})
		}
	}
	t.Run("link in a Git-ignored folder", func(t *testing.T) {
		_, repo := project(t)
		kinds[0].plant(t, filepath.Join(repo, "local", ".coopignore"))
		refused(t, repo, "local/.coopignore", kinds[0].reason)
	})
}

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
				if count, err := unscannedIgnoredCount(repo); count != 0 || err != nil {
					t.Errorf("hidden ignored descendants counted as visible: %d, %v", count, err)
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

// A launch, a build or any other command that meets a .coopignore it cannot read shows the same block
// as check-secrets, whatever the error was wrapped in on the way up, and exits 1. A box chooses the
// folder names in it, so anything a terminal would act on is shown escaped.
func TestEveryCommandDrawsAnUnreadableCoopignoreTheSameWay(t *testing.T) {
	policy := box.UnreadableCoopignore{File: "evil\x1b]0;PWNED\x07dir/.coopignore", Reason: "is not a regular file"}
	wrapped := fmt.Errorf("reading the build context of /repo: %w", policy)
	for _, tc := range []struct {
		name, then string
		draw       func() error
	}{
		{"a launch", "Then try again.", func() error { return wrapped }},
		{"coop build", "Then run coop build again.", func() error { return buildFailure("Could not build the box", wrapped, "docker", "coop build") }},
		{"coop update --box-only", "Then run coop update --box-only again.", func() error { return updateBoxFailure(wrapped, "docker", true) }},
		{"coop up", "Then run coop up again.", func() error {
			return composeRefusal("Could not start services from compose.yml", "compose.yml", wrapped, "coop up")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			out := captureStderr(t, func() { code = reportExit(-1, tc.draw()) })
			for _, want := range []string{
				`Could not read evil\u001B]0;PWNED\u0007dir/.coopignore`,
				`Remove evil\u001B]0;PWNED\u0007dir/.coopignore if you did not create it; otherwise make it a readable regular file of at most 1 MiB. ` + tc.then,
			} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
			if strings.ContainsAny(out, "\x1b\x07") {
				t.Errorf("the refusal printed a raw control character:\n%q", out)
			}
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
		})
	}
}

// A preset under a .coopignore coop cannot read fails to load naming that file, and a box chooses the
// folder names in the path and the detail, so both are shown escaped, alone and in the presets list.
func TestABrokenPresetIsShownEscaped(t *testing.T) {
	le := &preset.LoadError{Name: "team\x1b[2J", Path: "/repo/.agent/presets/team\x1b[2J/preset.yaml",
		Detail: []string{"Could not read evil\x1b]0;PWNED\x07dir/.coopignore: it is a folder."}}
	out := captureStderr(t, func() { reportExit(-1, presetLoadErr("/repo", "team", le)) })
	for _, want := range []string{`.agent/presets/team\u001B[2J/preset.yaml`, `Could not read evil\u001B]0;PWNED\u0007dir/.coopignore`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("the refusal printed a raw control character:\n%q", out)
	}

	cfg := freshConfig(t)
	writePresetFile(t, cfg.RepoOverride, "team\x1b[2J", "lead: [unclosed\n")
	list := captureStdout(t, func() { _, _ = (&app{cfg: cfg}).cmdPresets(nil) })
	if !strings.Contains(list, `team\u001B[2J`) || strings.Contains(list, "\x1b[2J") {
		t.Errorf("the presets list did not escape the broken preset's name and path:\n%q", list)
	}
}
