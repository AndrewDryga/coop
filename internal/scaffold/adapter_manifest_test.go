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
		{"none", nil, "207c39f1970cba0c8914bd5b4d58180de91d68a01c0a2e1800c1e42a34fded9d"},
		{"claude", []string{"claude"}, "95600316f64a30bbacf7e87b993d2fdfe8ac34d15403128fa674a2a8ced3f98b"},
		{"codex", []string{"codex"}, "fd7e3d35c0d10f753f3768a800c67d0477400a3f32fb4cd1232623c263e0db63"},
		{"gemini", []string{"gemini"}, "de75335e12075d4bd74049d9e05de25ee768a00ceb12df8550d74b3a8fe2977e"},
		{"all", []string{"claude", "codex", "gemini"}, "c6c53762e4beb0cc02f5277fd49907de818d6be8fe7d010f842456db9973b53c"},
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
