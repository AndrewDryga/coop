package box

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"golang.org/x/sys/unix"
)

type forkServiceTestFile struct {
	data []byte
	mode os.FileMode
}

func forkServiceFileFixture(t *testing.T, required, compose string, tracked map[string]forkServiceTestFile) (string, string, func(...string)) {
	t.Helper()
	gitEnv := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(gitEnv, "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(gitEnv, "system"))

	parent, git := gitrepo.New(t)
	writeForkServiceTestFile(t, parent, ".agent/project.yaml", []byte("services:\n  require_real_files:\n    - \""+required+"\"\n"), 0o644)
	writeForkServiceTestFile(t, parent, ".agent/compose.yml", []byte(compose), 0o644)
	for path, file := range tracked {
		writeForkServiceTestFile(t, parent, path, file.data, file.mode)
	}
	git("add", ".agent/project.yaml", ".agent/compose.yml")
	for path := range tracked {
		git("add", "--", path)
	}
	git("commit", "-qm", "fixture")

	fork := filepath.Join(t.TempDir(), "fork")
	git("clone", "-q", parent, fork)
	return parent, fork, git
}

func writeForkServiceTestFile(t *testing.T, root, path string, data []byte, mode os.FileMode) string {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		t.Fatal(err)
	}
	return full
}

func forkServiceGitResult(t *testing.T, repo string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = os.Environ()
	data, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(data)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("git %v: %v", args, err)
	}
	return exit.ExitCode(), string(data)
}

func TestForkServiceFilesCopyRequiredAndBoundSiblings(t *testing.T) {
	const required = "certs/generated/tls[1].key"
	compose := "services:\n  app:\n    image: alpine\n    volumes:\n      - ../certs/generated/tls[1].key:/key:ro\n      - ../certs/generated/tls.crt:/cert:ro\n"
	parent, fork, _ := forkServiceFileFixture(t, required, compose, nil)
	writeForkServiceTestFile(t, parent, required, []byte("generated key bytes\n"), 0o640)
	writeForkServiceTestFile(t, parent, "certs/generated/tls.crt", []byte("certificate bytes\n"), 0o644)
	writeForkServiceTestFile(t, parent, "certs/generated/unrelated.pem", []byte("unbound sibling\n"), 0o600)

	code, status := forkServiceGitResult(t, parent, "status", "--short", "--untracked-files=all")
	if code != 0 || !strings.Contains(status, "?? certs/generated/tls[1].key") {
		t.Fatalf("required source is not an untracked, non-ignored file: exit=%d, status=%q", code, status)
	}
	if err := CopyForkServiceFiles(parent, fork); err != nil {
		t.Fatal(err)
	}

	destKey := filepath.Join(fork, filepath.FromSlash(required))
	got, err := os.ReadFile(destKey)
	if err != nil || string(got) != "generated key bytes\n" {
		t.Fatalf("copied required file = %q, %v", got, err)
	}
	info, err := os.Stat(destKey)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("copied required file mode = %v, %v; want 0640", info, err)
	}
	if got, err := os.ReadFile(filepath.Join(fork, "certs/generated/tls.crt")); err != nil || string(got) != "certificate bytes\n" {
		t.Fatalf("copied bound sibling = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(fork, "certs/generated/unrelated.pem")); !os.IsNotExist(err) {
		t.Fatalf("unrelated sibling was copied: %v", err)
	}

	writeForkServiceTestFile(t, fork, "certs/generated/tls1.key", []byte("neighbor\n"), 0o600)
	for _, path := range []string{required, "certs/generated/tls.crt"} {
		code, output := forkServiceGitResult(t, fork, "check-ignore", "--no-index", "-q", "--", path)
		if code != 0 {
			t.Fatalf("copied untracked file %q is not locally Git-excluded: exit=%d, output=%q", path, code, output)
		}
	}
	code, output := forkServiceGitResult(t, fork, "check-ignore", "--no-index", "-q", "--", "certs/generated/tls1.key")
	if code != 1 {
		t.Fatalf("literal exclude also matched neighboring name: exit=%d, output=%q", code, output)
	}
	status = stringOutput(t, fork, "status", "--short", "--untracked-files=all")
	if !strings.Contains(status, "?? certs/generated/tls1.key") || strings.Contains(status, "tls[1].key") || strings.Contains(status, "tls.crt") {
		t.Fatalf("fork status does not show only the non-excluded neighbor: %q", status)
	}
}

func TestForkServiceFilesMissingRequiredParentFileIsSkipped(t *testing.T) {
	parent, fork, _ := forkServiceFileFixture(t, "certs/generated/tls.key", "services: {}\n", nil)
	if err := CopyForkServiceFiles(parent, fork); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(fork, "certs")); !os.IsNotExist(err) {
		t.Fatalf("missing required source left a destination entry: %v", err)
	}
}

func TestForkServiceFilesPreservesExistingDestinationFile(t *testing.T) {
	const path = "certs/generated/tls.key"
	parent, fork, _ := forkServiceFileFixture(t, path, "services: {}\n", map[string]forkServiceTestFile{
		path: {data: []byte("clone contents\n"), mode: 0o644},
	})
	writeForkServiceTestFile(t, parent, path, []byte("new generated contents\n"), 0o600)
	if err := os.Chmod(filepath.Join(fork, filepath.FromSlash(path)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopyForkServiceFiles(parent, fork); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(fork, filepath.FromSlash(path))
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "clone contents\n" {
		t.Fatalf("existing destination contents = %q, %v", got, err)
	}
	info, err := os.Stat(dest)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("existing destination mode = %v, %v; want 0644", info, err)
	}
}

func TestForkServiceFilesRefusesUnsafeRequiredSources(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, parent, path string) string
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, parent, path string) string {
				external := writeForkServiceTestFile(t, t.TempDir(), "external.key", []byte("outside key\n"), 0o600)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, path); err != nil {
					t.Fatal(err)
				}
				return external
			},
		},
		{
			name: "ancestor symlink",
			setup: func(t *testing.T, parent, path string) string {
				outside := filepath.Join(t.TempDir(), "generated")
				if err := os.Mkdir(outside, 0o755); err != nil {
					t.Fatal(err)
				}
				external := writeForkServiceTestFile(t, outside, "tls.key", []byte("outside key\n"), 0o600)
				if err := os.Symlink(filepath.Dir(outside), filepath.Join(parent, "certs")); err != nil {
					t.Fatal(err)
				}
				return external
			},
		},
		{
			name: "FIFO",
			setup: func(t *testing.T, parent, path string) string {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				return ""
			},
		},
		{
			name: "directory",
			setup: func(t *testing.T, parent, path string) string {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
				return writeForkServiceTestFile(t, path, "payload", []byte("outside key\n"), 0o600)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const required = "certs/generated/tls.key"
			parent, fork, _ := forkServiceFileFixture(t, required, "services: {}\n", nil)
			path := filepath.Join(parent, filepath.FromSlash(required))
			external := tc.setup(t, parent, path)
			if err := CopyForkServiceFiles(parent, fork); err == nil {
				t.Fatal("unsafe required source was accepted")
			}
			if _, err := os.Lstat(filepath.Join(fork, filepath.FromSlash(required))); !os.IsNotExist(err) {
				t.Fatalf("unsafe source created a destination entry: %v", err)
			}
			if external != "" {
				got, err := os.ReadFile(external)
				if err != nil || string(got) != "outside key\n" {
					t.Fatalf("external source changed: %q, %v", got, err)
				}
			}
		})
	}
}

func TestForkServiceFilesRefusesDestinationAncestorSymlink(t *testing.T) {
	const required = "certs/generated/tls.key"
	parent, fork, _ := forkServiceFileFixture(t, required, "services: {}\n", nil)
	writeForkServiceTestFile(t, parent, required, []byte("required key\n"), 0o600)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(fork, "certs")); err != nil {
		t.Fatal(err)
	}
	if err := CopyForkServiceFiles(parent, fork); err == nil {
		t.Fatal("destination ancestor symlink was accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("destination ancestor symlink wrote outside the fork: entries=%v, err=%v", entries, err)
	}
}

func TestForkServiceFilesRejectsRepositoryBookkeepingPaths(t *testing.T) {
	for _, path := range []string{".git/private.key", ".coop/private.key"} {
		t.Run(path, func(t *testing.T) {
			parent, fork, _ := forkServiceFileFixture(t, path, "services: {}\n", nil)
			writeForkServiceTestFile(t, parent, path, []byte("private source\n"), 0o600)
			err := CopyForkServiceFiles(parent, fork)
			if err == nil || !strings.Contains(err.Error(), "repository bookkeeping") {
				t.Fatalf("bookkeeping path error = %v, want repository bookkeeping refusal", err)
			}
			if _, err := os.Lstat(filepath.Join(fork, filepath.FromSlash(path))); !os.IsNotExist(err) {
				t.Fatalf("bookkeeping source was copied: %v", err)
			}
		})
	}
}

func stringOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	code, output := forkServiceGitResult(t, repo, args...)
	if code != 0 {
		t.Fatalf("git %v: exit=%d, output=%q", args, code, output)
	}
	return output
}
