package eval

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScaffoldWritesAWorkingSuiteAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := Scaffold(dir); err != nil {
		t.Fatal(err)
	}
	// The scaffolded suite loads and validates as-is.
	if _, err := Load(filepath.Join(dir, "suite.yaml")); err != nil {
		t.Fatalf("scaffolded suite does not load: %v", err)
	}
	// Create-only: a second scaffold into the same dir refuses rather than truncating a kept file.
	if err := Scaffold(dir); err == nil {
		t.Fatal("Scaffold overwrote an existing file")
	}
	// Even with suite.yaml gone but another file kept, the kept file is not clobbered.
	if err := os.Remove(filepath.Join(dir, "suite.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := Scaffold(dir); err == nil {
		t.Fatal("Scaffold overwrote a kept file after suite.yaml was removed")
	}
}

// A scaffolded suite is named after its folder, so coop eval runs tells two custom suites apart.
// The name is written as a YAML string, so a folder name YAML would read as something else stays
// the name; a folder with no name of its own (the filesystem root) keeps the generic one.
func TestScaffoldNamesTheSuiteAfterItsFolder(t *testing.T) {
	for folder, want := range map[string]string{
		"nightly-checks": "nightly-checks",
		"my suite":       "my suite",
		"a: b #c":        "a: b #c",
		"true":           "true",
	} {
		dir := filepath.Join(t.TempDir(), folder)
		if err := Scaffold(dir); err != nil {
			t.Fatal(err)
		}
		suite, err := Load(filepath.Join(dir, "suite.yaml"))
		if err != nil {
			t.Fatalf("scaffold in %q does not load: %v", folder, err)
		}
		if suite.Name != want {
			t.Errorf("scaffold in %q named the suite %q, want %q", folder, suite.Name, want)
		}
	}
	if got := suiteName(string(filepath.Separator)); got != "my-suite" {
		t.Errorf("suiteName(root) = %q, want the generic my-suite", got)
	}
}

func TestScaffoldRefusesKeptFileBeforeCreatingOtherFiles(t *testing.T) {
	dir := t.TempDir()
	kept := filepath.Join(dir, "files", "greeting", ".keep")
	if err := os.MkdirAll(filepath.Dir(kept), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Scaffold(dir); err == nil {
		t.Fatal("Scaffold accepted a kept file")
	}
	for _, rel := range []string{"suite.yaml", "verifiers/greeting/README.md", "verifiers/greeting/verify.sh"} {
		if _, err := os.Lstat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Errorf("failed scaffold created %s: %v", rel, err)
		}
	}
	if body, err := os.ReadFile(kept); err != nil || string(body) != "mine" {
		t.Fatalf("kept file was changed: %q, %v", body, err)
	}
}

// The scaffolded verifier grades its working directory, which is the candidate's workspace both in
// an ordinary case (/workspace) and in a runtime profile case (/app), so it never names either path.
func TestScaffoldVerifierGradesItsWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := Scaffold(dir); err != nil {
		t.Fatal(err)
	}
	verifier := filepath.Join(dir, "verifiers", "greeting", "verify.sh")
	for _, tc := range []struct {
		name, content, want string
		code                int
	}{
		{name: "correct", content: "hello", want: "greeting.txt is correct", code: 0},
		{name: "wrong", content: "hi", want: "does not contain exactly: hello", code: 1},
		{name: "missing", want: "greeting.txt was not created", code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir() // not /workspace: the grader's directory is where the work is
			if tc.content != "" {
				if err := os.WriteFile(filepath.Join(workspace, "greeting.txt"), []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", verifier)
			cmd.Dir = workspace
			out, err := cmd.CombinedOutput()
			code := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.code || !strings.Contains(string(out), tc.want) {
				t.Fatalf("verify.sh in %s = exit %d, %q; want exit %d with %q", tc.name, code, out, tc.code, tc.want)
			}
		})
	}
}
