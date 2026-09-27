//go:build darwin || linux

package box

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestNewShadowDeciderRefusesUnboundedOrBlockingPolicyFiles(t *testing.T) {
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
			if _, err := file.WriteString("private.txt\n"); err != nil {
				_ = file.Close()
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
			if err := os.WriteFile(outside, []byte("private.txt\n"), 0o600); err != nil {
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
			type decision struct{ private, builtin bool }
			done := make(chan decision, 1)
			go func() {
				hidden := NewShadowDecider(repo)
				done <- decision{hidden("private.txt"), hidden(".env")}
			}()
			select {
			case got := <-done:
				if got.private || !got.builtin {
					t.Fatalf("unsafe policy changed visibility: %+v", got)
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
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, CoopIgnoreFile), []byte("private.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !NewShadowDecider(repo)("private.txt") {
		t.Fatal("valid project policy did not hide its requested file")
	}
}
