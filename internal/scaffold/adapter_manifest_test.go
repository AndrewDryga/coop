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
		{"none", nil, "69c8114ae74e47a440b16f901e8522a984d8e23412af519113f9826c12cd5a89"},
		{"claude", []string{"claude"}, "de98bcd07cd5fc7d7a57483263ba77b9710aedd541cf6f78b966de538b1cf67f"},
		{"codex", []string{"codex"}, "cd049e3bf45315845f2026ca591253a47b6965a7791951f0ea461fa02395cf8a"},
		{"gemini", []string{"gemini"}, "ab0386149680024d3de5ca9084491aa57c458503094c7053265f6b644b88bb2b"},
		{"all", []string{"claude", "codex", "gemini"}, "0a3c4c9d8fcec72cd359b36a286fbd9e350eeb2f5cd438d65734aa0acf907930"},
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
