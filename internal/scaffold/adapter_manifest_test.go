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

// Pin the complete pre-refactor output, including links, modes and hook bytes.
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
		{"none", nil, "185caeb9d70b8a6d1ffcca04280900ea64962b6b4e0cb2006caa5a6012ef2eb3"},
		{"claude", []string{"claude"}, "db633c557e4cedb687714bbc8bc195e840ee25b9f22387d668be7227e7c49873"},
		{"codex", []string{"codex"}, "db7ea275f134ef6934c2351b35bdb4eb431c82035c76c0d270437e801032c201"},
		{"gemini", []string{"gemini"}, "dba11e545dd76c112ae19d35385925112ce0f18242ee030ae275ef2ace29bc34"},
		{"all", []string{"claude", "codex", "gemini"}, "6f64035caf4f5d97341b184b4113759d65da0f4d39e61aa2336c0bff93891e0c"},
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
