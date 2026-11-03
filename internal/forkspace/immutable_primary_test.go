package forkspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnknownPrimaryReceiptSurvivesStagedPurge(t *testing.T) {
	repo := t.TempDir()
	name := "primary"
	workspace := Workspace(repo, name)
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "payload"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecordImmutablePrimarySnapshot(workspace, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := StageWorkspaceDiscardLocked(repo, name, info)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, ".git", ImmutablePrimaryMarker), []byte(`{"version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	count, err := PurgeStagedDiscards(repo, name)
	if err == nil || count != 0 {
		t.Fatalf("unknown primary purge = %d %v", count, err)
	}
	if data, err := os.ReadFile(filepath.Join(staged, "payload")); err != nil || string(data) != "keep" {
		t.Fatal("unknown staged payload lost")
	}
}
