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
		{"none", nil, "2196ece97a820f7a33fec39e994eb7b28e3220eeb4aa64a8f2a61aae351d4cb6"},
		{"claude", []string{"claude"}, "860a62b99177c219a737cfd82d3ee1be82c5789f35e9ef4f449db3706fe0efd0"},
		{"codex", []string{"codex"}, "d31e214578b75cd08a2df0672d40ad7e9997ec373c555b387a98a83910aa916d"},
		{"gemini", []string{"gemini"}, "bef93e7645481b0944c55f9d2469291160e962a0021ed92bd91b0fe13b251f5c"},
		{"all", []string{"claude", "codex", "gemini"}, "11590943a5cff6e7458d2b1a85e071465247b5d6fba0809eda56a55127cedcca"},
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
