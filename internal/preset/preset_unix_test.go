//go:build darwin || linux

package preset

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoadRefusesPresetFIFOsWithoutBlocking(t *testing.T) {
	for _, rel := range []string{"preset.yaml", "lead.md"} {
		t.Run(rel, func(t *testing.T) {
			repo := t.TempDir()
			dir := filepath.Join(repo, filepath.FromSlash(Dir), "unsafe")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if rel != "preset.yaml" {
				if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte("lead: {agent: codex, prompt: lead.md}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, rel)
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := Load(repo, "", "unsafe")
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "safely") {
					t.Fatalf("FIFO preset error = %v", err)
				}
			case <-time.After(2 * time.Second):
				if unblock, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
					_ = unblock.Close()
				}
				t.Fatal("preset load blocked on a repository FIFO")
			}
		})
	}
}

func TestLoadRejectsOversizedPresetFile(t *testing.T) {
	repo := writePreset(t, "large", strings.Repeat("#", presetFileLimit+1), nil)
	if _, err := Load(repo, "", "large"); err == nil || !strings.Contains(err.Error(), "safely") {
		t.Fatalf("oversized preset error = %v", err)
	}
}
