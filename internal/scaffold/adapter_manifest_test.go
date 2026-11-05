package scaffold

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// Pin the complete scaffold output, including links, modes and hook bytes.
// Expectations are independent of the adapter descriptors they qualify.
func TestInitAdapterManifest(t *testing.T) {
	// File creation honors umask. Set it only in an isolated test process, never
	// globally in this process where other tests may run concurrently.
	if os.Getenv("COOP_ADAPTER_MANIFEST_CHILD") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `umask 022 || exit; exec "$@"`, "manifest", binary, "-test.run=^TestInitAdapterManifest$")
		cmd.Env = append(os.Environ(), "COOP_ADAPTER_MANIFEST_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("scaffold manifest: %v\n%s", err, out)
		}
		return
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	for _, tc := range []struct {
		name   string
		agents []string
		want   string
	}{
		{"none", nil, "403d6fc5b1939b0afbf1e1b4191a86e95b88efbcdc1657ce345f2f3342cd729c"},
		{"claude", []string{"claude"}, "01831f5a766f5067e8ca4536e6f9fc58d9439320d80347aecf5ff9e43ed20ea5"},
		{"codex", []string{"codex"}, "b80d677c4fff12dfc7d0c90f0ec64fb9fd73f87a9cf1b51f48f980dfa08de6dd"},
		{"gemini", []string{"gemini"}, "4eeb8d415ee809baeccde9629828eb3554357d9d1668160f9b4ef76998ef17a2"},
		{"all", []string{"claude", "codex", "gemini"}, "68ba44c052b9ec175259229c707722b4f04c3cf5b963a67b4502d9d275b80b09"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if _, err := Init(repo, "", []string{"go"}, tc.agents); err != nil {
				t.Fatal(err)
			}
			h := sha256.New()
			err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(repo, path)
				if err != nil {
					return err
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				mode := info.Mode()
				// Link permission bits are not portable (macOS 0755, Linux 0777);
				// the link type and exact target are the meaningful contract.
				if mode&os.ModeSymlink != 0 {
					mode = os.ModeSymlink
				}
				fmt.Fprintf(h, "%s\x00%s\x00", filepath.ToSlash(rel), mode)
				switch {
				case info.Mode()&os.ModeSymlink != 0:
					link, err := os.Readlink(path)
					if err != nil {
						return err
					}
					fmt.Fprint(h, link)
				case info.Mode().IsRegular():
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					_, _ = h.Write(data)
				}
				fmt.Fprint(h, "\x00")
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprintf("%x", h.Sum(nil)); got != tc.want {
				t.Errorf("manifest = %s, want %s", got, tc.want)
			}
		})
	}
}
