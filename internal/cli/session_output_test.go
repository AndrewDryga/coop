package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectSessionOutputDirAcceptsImagesAndRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	data := []byte("\x89PNG\r\n\x1a\nchart")
	if err := os.WriteFile(filepath.Join(dir, "load.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := collectSessionOutputDir(dir)
	if err != nil || len(artifacts) != 1 || artifacts[0].Name != "load.png" || string(artifacts[0].Data) != string(data) {
		t.Fatalf("artifacts = %+v err=%v", artifacts, err)
	}
	if err := os.Symlink(filepath.Join(dir, "load.png"), filepath.Join(dir, "leak.png")); err != nil {
		t.Fatal(err)
	}
	if _, err := collectSessionOutputDir(dir); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("symlink output error = %v", err)
	}
}

func TestPrepareSessionOutputDirDoesNotReplaceExistingContent(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, sessionOutputRoot)
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "turn-1")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareSessionOutputDir(workspace, "turn-1"); err == nil {
		t.Fatal("existing turn output directory was replaced")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("existing turn output directory was removed: %v", err)
	}
}
