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

// A .coopignore coop cannot read on the way to a preset, or to a prompt file the preset names, leaves
// unknown whether the file is hidden: the load refuses, naming that .coopignore and the fix, rather
// than blaming the preset.
func TestLoadNamesAnUnreadableCoopignoreOnTheWay(t *testing.T) {
	for _, tc := range []struct{ name, policyDir string }{
		{"in the preset folder", "unsafe"},
		{"in the prompt's folder", "unsafe/prompts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			presets := filepath.Join(repo, filepath.FromSlash(Dir))
			dir := filepath.Join(presets, "unsafe")
			if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte("lead: {agent: codex, prompt: prompts/lead.md}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "prompts", "lead.md"), []byte("Lead.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(presets, filepath.FromSlash(tc.policyDir), ".coopignore"), 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := Load(repo, "", "unsafe")
			file := filepath.ToSlash(filepath.Join(Dir, tc.policyDir, ".coopignore"))
			for _, want := range []string{
				"Coop cannot tell what " + file + " hides, because it is a folder.",
				"Remove " + file + " if you did not create it",
			} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("Load = %v; want %q", err, want)
				}
			}
		})
	}
}
