package box

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestForkServiceFilesRootBindAndInvalidCompose(t *testing.T) {
	for name, compose := range map[string]string{
		"root bind":       "services:\n  app:\n    image: alpine\n    volumes: ['../:/repo:ro']\n",
		"invalid compose": "services: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			parent, fork, _ := forkServiceFileFixture(t, "dev.key", compose, nil)
			writeForkServiceTestFile(t, parent, "dev.key", []byte("generated\n"), 0o600)
			if err := CopyForkServiceFiles(parent, fork); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(fork, "dev.key")); err != nil || string(got) != "generated\n" {
				t.Fatalf("required file = %q, %v", got, err)
			}
		})
	}
}

func TestForkServiceFilesDirectoryPermissions(t *testing.T) {
	for _, tc := range []struct {
		mode os.FileMode
		mask int
	}{{0o750, 0o077}, {0o700, 0o022}, {0o555, 0o077}} {
		t.Run(fmt.Sprintf("mode-%o-mask-%o", tc.mode, tc.mask), func(t *testing.T) {
			parent, fork, _ := forkServiceFileFixture(t, "certs/tls.key", "services:\n  app:\n    image: alpine\n    volumes: ['../certs/tls.crt:/cert:ro']\n", nil)
			writeForkServiceTestFile(t, parent, "certs/tls.key", []byte("key\n"), 0o600)
			writeForkServiceTestFile(t, parent, "certs/tls.crt", []byte("cert\n"), 0o644)
			parentDir, forkDir := filepath.Join(parent, "certs"), filepath.Join(fork, "certs")
			t.Cleanup(func() { _ = os.Chmod(parentDir, 0o700); _ = os.Chmod(forkDir, 0o700) })
			if err := os.Chmod(parentDir, tc.mode); err != nil {
				t.Fatal(err)
			}
			previous := unix.Umask(tc.mask)
			err := CopyForkServiceFiles(parent, fork)
			unix.Umask(previous)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(forkDir)
			if err != nil || info.Mode().Perm() != tc.mode|0o200 {
				t.Fatalf("copied directory = %v, %v; want mode %o", info, err, tc.mode|0o200)
			}
			for _, name := range []string{"tls.key", "tls.crt"} {
				if _, err := os.ReadFile(filepath.Join(forkDir, name)); err != nil {
					t.Fatal(err)
				}
			}
			home, err := os.OpenRoot(filepath.Dir(fork))
			if err != nil {
				t.Fatal(err)
			}
			defer home.Close()
			if err := home.RemoveAll(filepath.Base(fork)); err != nil {
				t.Fatalf("remove copied fork: %v", err)
			}
		})
	}
}

func TestForkServiceFilesAbsoluteSiblingThroughParentAlias(t *testing.T) {
	parent, fork, _ := forkServiceFileFixture(t, "certs/tls.key", "services: {}\n", nil)
	alias := filepath.Join(t.TempDir(), "parent-alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	writeForkServiceTestFile(t, parent, "certs/tls.key", []byte("key\n"), 0o600)
	writeForkServiceTestFile(t, parent, "certs/tls.crt", []byte("cert\n"), 0o644)
	compose := fmt.Sprintf("services:\n  app:\n    image: alpine\n    volumes:\n      - type: bind\n        source: %q\n        target: /cert\n        read_only: true\n", filepath.Join(alias, "certs", "tls.crt"))
	writeForkServiceTestFile(t, parent, ".agent/compose.yml", []byte(compose), 0o644)
	if err := CopyForkServiceFiles(parent, fork); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(fork, "certs", "tls.crt")); err != nil || string(got) != "cert\n" {
		t.Fatalf("sibling through parent alias = %q, %v", got, err)
	}
}
