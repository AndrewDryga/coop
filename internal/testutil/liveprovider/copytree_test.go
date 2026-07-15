package liveprovider

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCopyRegularTreePublishesPrivateBoundedCopy(t *testing.T) {
	sourceRoot := t.TempDir()
	source := filepath.Join(sourceRoot, "presets", "frontier")
	mustMkdir(t, filepath.Join(source, "nested"))
	writeSource(t, filepath.Join(source, "preset.yaml"), "lead: codex\n", 0o644)
	writeSource(t, filepath.Join(source, "nested", "role.md"), "read only\n", 0o600)
	destination := filepath.Join(t.TempDir(), ".agent", "presets", "frontier")

	if err := CopyRegularTree(sourceRoot, source, destination, 1024); err != nil {
		t.Fatal(err)
	}
	for relative, want := range map[string]string{
		"preset.yaml": "lead: codex\n", "nested/role.md": "read only\n",
	} {
		path := filepath.Join(destination, relative)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", relative, data, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", relative, info.Mode().Perm())
		}
	}
	if info, err := os.Stat(filepath.Join(destination, "nested")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("nested directory = (%v, %v), want mode 700", info, err)
	}
}

func TestCopyRegularTreeRejectsUnsafeSourcesWithoutPublishing(t *testing.T) {
	tests := []struct {
		name     string
		maxBytes int64
		build    func(*testing.T, string, string)
	}{
		{name: "child symlink", maxBytes: 1024, build: func(t *testing.T, root, source string) {
			outside := filepath.Join(root, "CANARY")
			writeSource(t, outside, "SECRET_CANARY", 0o600)
			mustMkdir(t, source)
			if err := os.Symlink(outside, filepath.Join(source, "preset.yaml")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", maxBytes: 1024, build: func(t *testing.T, _, source string) {
			mustMkdir(t, source)
			original := filepath.Join(source, "preset.yaml")
			writeSource(t, original, "lead: codex\n", 0o600)
			if err := os.Link(original, filepath.Join(source, "alias.yaml")); err != nil {
				t.Skipf("hardlinks unavailable: %v", err)
			}
		}},
		{name: "fifo", maxBytes: 1024, build: func(t *testing.T, _, source string) {
			mustMkdir(t, source)
			if err := syscall.Mkfifo(filepath.Join(source, "preset.yaml"), 0o600); err != nil {
				t.Skipf("fifo unavailable: %v", err)
			}
		}},
		{name: "aggregate size", maxBytes: 5, build: func(t *testing.T, _, source string) {
			mustMkdir(t, source)
			writeSource(t, filepath.Join(source, "one"), "abc", 0o600)
			writeSource(t, filepath.Join(source, "two"), "def", 0o600)
		}},
		{name: "unsafe mode", maxBytes: 1024, build: func(t *testing.T, _, source string) {
			mustMkdir(t, source)
			writeSource(t, filepath.Join(source, "preset.yaml"), "lead: codex\n", 0o666)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sourceRoot := t.TempDir()
			source := filepath.Join(sourceRoot, "presets", "frontier")
			tt.build(t, sourceRoot, source)
			destination := filepath.Join(t.TempDir(), "copy")
			if err := CopyRegularTree(sourceRoot, source, destination, tt.maxBytes); err == nil {
				t.Fatal("unsafe regular tree was accepted")
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial destination remains: %v", err)
			}
		})
	}
}

func TestCopyRegularTreeRejectsSymlinkRootAndExistingDestination(t *testing.T) {
	sourceRoot := t.TempDir()
	realSource := filepath.Join(sourceRoot, "real")
	mustMkdir(t, realSource)
	writeSource(t, filepath.Join(realSource, "preset.yaml"), "lead: codex\n", 0o600)
	linkedSource := filepath.Join(sourceRoot, "frontier")
	if err := os.Symlink(realSource, linkedSource); err != nil {
		t.Fatal(err)
	}
	if err := CopyRegularTree(sourceRoot, linkedSource, filepath.Join(t.TempDir(), "copy"), 1024); err == nil {
		t.Fatal("symlinked regular tree root was accepted")
	}

	destination := filepath.Join(t.TempDir(), "existing")
	mustMkdir(t, destination)
	if err := CopyRegularTree(sourceRoot, realSource, destination, 1024); err == nil {
		t.Fatal("existing regular tree destination was replaced")
	}
}
