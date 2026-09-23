//go:build darwin || linux

package box

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLoadUserGlobsRefusesUnboundedOrBlockingPolicyFiles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(*testing.T, string)
	}{
		{"fifo", func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized", func(t *testing.T, path string) {
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(coopIgnoreSnapshotLimit + 1); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			_ = file.Close()
		}},
		{"outward symlink", func(t *testing.T, path string) {
			outside := filepath.Join(t.TempDir(), "policy")
			if err := os.WriteFile(outside, []byte("*.secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			path := filepath.Join(repo, CoopIgnoreFile)
			tc.plant(t, path)
			done := make(chan UserGlobs, 1)
			go func() { done <- LoadUserGlobs(repo) }()
			select {
			case got := <-done:
				if len(got.Base) != 0 || len(got.Path) != 0 {
					t.Fatalf("unsafe policy produced globs: %+v", got)
				}
			case <-time.After(2 * time.Second):
				if tc.name == "fifo" {
					if unblock, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
						_ = unblock.Close()
					}
				}
				t.Fatal("unsafe .coopignore blocked policy loading")
			}
		})
	}
}
