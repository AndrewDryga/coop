//go:build darwin || linux

package forkspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExcludeRetryConfirmsVisiblePatternDurability(t *testing.T) {
	repo := committedSetupRepo(t)
	previousFile, previousDirectory := syncLocalExcludeFile, syncLocalExcludeDirectory
	t.Cleanup(func() {
		syncLocalExcludeFile = previousFile
		syncLocalExcludeDirectory = previousDirectory
	})
	failure := errors.New("synthetic exclude file sync failure")
	syncLocalExcludeFile = func(*os.File) error { return failure }
	pattern := "/.coop-network-approval"
	if err := Exclude(repo, pattern); !errors.Is(err, failure) {
		t.Fatalf("first Exclude = %v, want %v", err, failure)
	}

	fileSyncs, directorySyncs := 0, 0
	syncLocalExcludeFile = func(file *os.File) error {
		fileSyncs++
		return file.Sync()
	}
	syncLocalExcludeDirectory = func(dir *os.File) error {
		directorySyncs++
		return dir.Sync()
	}
	if err := Exclude(repo, pattern); err != nil {
		t.Fatal(err)
	}
	if fileSyncs != 1 || directorySyncs != 1 {
		t.Fatalf("existing pattern retry barriers = file %d, directory %d; want 1 each", fileSyncs, directorySyncs)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(data), pattern); count != 1 {
		t.Fatalf("exclude pattern count = %d, want 1:\n%s", count, data)
	}
}
