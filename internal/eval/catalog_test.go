package eval

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every shipped starter must actually load and validate. A starter that does not parse is worse
// than no starter: it fails at the moment a new user first tries the feature.
func TestEveryShippedStarterLoads(t *testing.T) {
	if len(Starters()) == 0 {
		t.Fatal("the catalog is empty")
	}
	for _, s := range Starters() {
		t.Run(s.ID, func(t *testing.T) {
			if strings.TrimSpace(s.Summary) == "" {
				t.Error("no summary; `coop eval ls` would show a blank line")
			}
			path, ok, err := StarterPath(s.ID, t.TempDir())
			if err != nil || !ok {
				t.Fatalf("StarterPath: ok=%v err=%v", ok, err)
			}
			suite, err := Load(path)
			if err != nil {
				t.Fatalf("the shipped starter does not load: %v", err)
			}
			if len(suite.Cases) == 0 {
				t.Error("a starter with no cases measures nothing")
			}
			// Each case's inputs and its hidden verifier must exist, or the first run errors.
			for _, c := range suite.Cases {
				if _, err := os.Stat(filepath.Join(suite.Dir, c.Verifier)); err != nil {
					t.Errorf("case %q verifier is missing: %v", c.ID, err)
				}
				entry := filepath.Join(suite.Dir, c.Verifier, "verify.sh")
				body, err := os.ReadFile(entry)
				if err != nil {
					t.Errorf("case %q has no verify.sh: %v", c.ID, err)
					continue
				}
				// The three-valued contract is what keeps a broken grader from reading as a model
				// failure, so a shipped verifier must actually use exit 1 for "did not pass".
				if !strings.Contains(string(body), "exit 1") {
					t.Errorf("case %q verifier never fails a candidate; it cannot discriminate", c.ID)
				}
			}
		})
	}
}

func TestStarterProtectsNewAndExistingEvalRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "eval")
	for _, existing := range []bool{false, true} {
		if existing {
			if err := os.Chmod(root, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if _, ok, err := StarterPath("core", root); err != nil || !ok {
			t.Fatalf("StarterPath(existing=%v): ok=%v err=%v", existing, ok, err)
		}
		info, err := os.Stat(root)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("eval root mode after existing=%v: %v, %v; want 0700", existing, info, err)
		}
	}
}

func TestStarterRefusesLinkedEvalRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "eval")
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := StarterPath("core", root); err == nil {
		t.Fatal("starter extracted through a linked eval state root")
	}
}

// The unpacked copy belongs to the binary: a stale one from an older version would silently change
// what a comparison measures.
func TestStarterIsRewrittenNotReused(t *testing.T) {
	root := t.TempDir()
	path, _, err := StarterPath("core", root)
	if err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(filepath.Dir(path), "stray-from-an-older-version.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := StarterPath("core", root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Error("a stale file survived re-unpacking the starter")
	}
}

// A shell fixture the instruction tells the model to run has to be runnable: embed carries content,
// not modes.
func TestStarterShellFilesAreExecutable(t *testing.T) {
	path, _, err := StarterPath("core", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	found := 0
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".sh") {
			return err
		}
		found++
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Error("no shell files were unpacked")
	}
}

func TestUnknownStarterIsNotAStarter(t *testing.T) {
	if _, ok, err := StarterPath("no-such-starter", t.TempDir()); ok || err != nil {
		t.Errorf("ok=%v err=%v; an unknown id must not resolve", ok, err)
	}
}

func TestCoreVerifierRejectsCorrectOutputWithFailedCommand(t *testing.T) {
	path, _, err := StarterPath("core", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(path)
	workspace := t.TempDir()
	// The first checked command prints the expected bytes, but reports failure.
	if err := os.WriteFile(filepath.Join(workspace, "greet.sh"), []byte("#!/bin/sh\necho 'HELLO, WORLD!'\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(root, "verifiers/keep-the-contract/verify.sh"))
	cmd.Dir = workspace
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "--upper:") {
		t.Fatalf("verifier accepted failed command or gave no useful reason: err=%v output=%q", err, output)
	}
}

func TestQueueVerifierChecksOriginalTestBehavior(t *testing.T) {
	path, _, err := StarterPath("queue", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(path)
	workspace := filepath.Join(root, "fixtures/toolkit")
	verifier := filepath.Join(root, "verifiers/ten-tasks/verify.sh")
	testPath := filepath.Join(workspace, "test.sh")
	original, err := os.ReadFile(testPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, test  string
		wantMissing bool
		toolMode    os.FileMode
	}{
		{"original", string(original), false, 0o755},
		{"commented out", strings.Replace(string(original), `expect "upper" "ABC" upper "abc"`, `# expect "upper" "ABC" upper "abc"`, 1), true, 0o755},
		{"equivalent quotes", strings.Replace(string(original), `expect "upper" "ABC" upper "abc"`, `expect 'upper' 'ABC' upper 'abc'`, 1), false, 0o755},
		{"absolute shell", strings.Replace(string(original), "sh ./tools.sh", "/bin/sh ./tools.sh", 1), false, 0o755},
		{"non-executable toolkit", string(original), false, 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(testPath, []byte(tc.test), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(workspace, "tools.sh"), tc.toolMode); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", verifier)
			cmd.Dir = workspace
			output, err := cmd.CombinedOutput() // the other nine commands are intentionally unfinished
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 ||
				!strings.Contains(string(output), "0/10 subcommands correct") ||
				strings.Contains(string(output), "test.sh fails:") ||
				strings.Contains(string(output), "the subcommand that already worked is broken") {
				t.Fatalf("verifier did not finish grading the unfinished toolkit: err=%v output=%q", err, output)
			}
			missing := strings.Contains(string(output), "test.sh no longer tests the original behaviour")
			if missing != tc.wantMissing {
				t.Fatalf("missing original test=%v, want %v; output=%q", missing, tc.wantMissing, output)
			}
		})
	}
}
