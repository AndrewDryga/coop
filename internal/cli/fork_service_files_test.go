package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestForkServiceFilesCopyRequiredAndBoundFiles(t *testing.T) {
	for _, isolated := range []bool{false, true} {
		name := "ordinary"
		if isolated {
			name = "isolated"
		}
		t.Run(name, func(t *testing.T) {
			repo := initRepo(t)
			seedForkNetworkPreflightTask(t, repo)
			writeForkNetworkPreflightProject(t, repo)
			projectPath := filepath.Join(repo, project.File)
			projectData, err := os.ReadFile(projectPath)
			if err != nil {
				t.Fatal(err)
			}
			projectData = append(projectData, []byte("services:\n  require_real_files:\n    - dev/certs/tls.key\n")...)
			if err := os.WriteFile(projectPath, projectData, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("dev/certs/\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			compose := filepath.Join(repo, ".agent", "compose.yml")
			if err := os.WriteFile(compose, []byte("services:\n  tls:\n    image: example/tls\n    volumes:\n      - ../dev/certs/tls.key:/run/tls.key:ro\n      - ../dev/certs/tls.crt:/run/tls.crt:ro\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			certs := filepath.Join(repo, "dev", "certs")
			if err := os.MkdirAll(certs, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(certs, "tls.key"), []byte("parent key\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(certs, "tls.crt"), []byte("parent certificate\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(certs, "unbound.pem"), []byte("unbound sibling\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			commitForkNetworkPreflightFixture(t, repo)

			a := forkNetworkPreflightApp(t, repo)
			args := []string{name, "claude", "--loop", "-d"}
			if isolated {
				args = append(args, "--isolated")
			}
			// Match the detached refusal fixture: if the parent ever detaches before its own
			// preflight, wait for this fixture worker to refuse and clear its PID before cleanup.
			t.Setenv(detachedChildEnv, "1")
			t.Cleanup(func() {
				if !pathExists(forkspace.PidPath(repo, name)) {
					return
				}
				code, err := a.forkctl().ForkStop([]string{name})
				if code != 0 || err != nil {
					t.Errorf("cleanup fork stop = (%d, %v)", code, err)
				}
				if pathExists(forkspace.PidPath(repo, name)) {
					t.Errorf("cleanup left a worker pid record for fixture fork %s", name)
				}
			})
			var code int
			var runErr error
			var stderr string
			stdout := captureStdout(t, func() {
				stderr = captureStderr(t, func() {
					code, runErr = a.forkCreate(args)
				})
			})
			output := stdout + stderr
			if strings.Contains(output, "Started fork") {
				wait.For(t, "detached fixture worker approval refusal", func() bool {
					data, err := os.ReadFile(forkspace.LogPath(repo, name))
					return err == nil && strings.Contains(string(data), forkReview(t, repo, name))
				})
				wait.For(t, "detached fixture worker pid record to clear", func() bool {
					return !pathExists(forkspace.PidPath(repo, name))
				})
			}
			if code != 1 || runErr == nil || !strings.Contains(runErr.Error(), forkReview(t, repo, name)) {
				t.Fatalf("fork creation = (%d, %v), want the fixture's network approval refusal after creation", code, runErr)
			}
			if strings.Contains(output, "Started fork") {
				t.Errorf("detached fork printed a successful start before approval:\n%s", output)
			}
			if pathExists(forkspace.PidPath(repo, name)) {
				t.Error("refused detached fork left a worker pid record")
			}

			ws := forkspace.Workspace(repo, name)
			parentKey := filepath.Join(certs, "tls.key")
			parentCert := filepath.Join(certs, "tls.crt")
			forkKey := filepath.Join(ws, "dev", "certs", "tls.key")
			forkCert := filepath.Join(ws, "dev", "certs", "tls.crt")
			for _, file := range []struct {
				parent string
				fork   string
				want   string
			}{
				{parentKey, forkKey, "parent key\n"},
				{parentCert, forkCert, "parent certificate\n"},
			} {
				got, err := os.ReadFile(file.fork)
				if err != nil || string(got) != file.want {
					t.Errorf("fork copy %s = %q, %v; want %q", file.fork, got, err, file.want)
					continue
				}
				parentInfo, parentErr := os.Stat(file.parent)
				forkInfo, forkErr := os.Stat(file.fork)
				if parentErr != nil || forkErr != nil {
					t.Errorf("fork copy %s is not an independent file (parent stat %v, fork stat %v)", file.fork, parentErr, forkErr)
				} else if os.SameFile(parentInfo, forkInfo) {
					t.Errorf("fork copy %s shares the parent file inode", file.fork)
				}
			}
			if _, err := os.Lstat(filepath.Join(ws, "dev", "certs", "unbound.pem")); !os.IsNotExist(err) {
				t.Errorf("unbound sibling was copied into fork: %v", err)
			}

			if err := os.WriteFile(parentKey, []byte("changed parent key\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(parentCert, []byte("changed parent certificate\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			code, err = a.forkCreate([]string{name})
			if code != 1 || err == nil || !strings.Contains(err.Error(), forkReview(t, repo, name)) {
				t.Fatalf("fork reopen = (%d, %v), want the fixture's network approval refusal", code, err)
			}
			for _, file := range []struct {
				path string
				want string
			}{
				{forkKey, "parent key\n"},
				{forkCert, "parent certificate\n"},
			} {
				got, err := os.ReadFile(file.path)
				if err != nil || string(got) != file.want {
					t.Errorf("reopened fork file %s = %q, %v; want unchanged %q", file.path, got, err, file.want)
				}
			}
		})
	}
}
