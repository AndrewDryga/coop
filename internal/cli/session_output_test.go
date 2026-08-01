package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
)

func TestCollectSessionOutputDirAcceptsImagesAndRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	data := []byte("\x89PNG\r\n\x1a\nchart")
	if err := os.WriteFile(filepath.Join(dir, "load.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.csv"), []byte("time,value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".venv"), 0o700); err != nil {
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

func TestCollectSessionOutputDirCountsOnlyImageCandidates(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"source.csv", "notes.txt", "chart.json", "scratch.log", "table.tsv"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("scratch"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("\x89PNG\r\n\x1a\nchart")
	if err := os.WriteFile(filepath.Join(dir, "load.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := collectSessionOutputDir(dir)
	if err != nil || len(artifacts) != 1 || artifacts[0].Name != "load.png" {
		t.Fatalf("artifacts = %+v err=%v", artifacts, err)
	}
}

func TestCollectSessionOutputDirStillBoundsImageCandidates(t *testing.T) {
	dir := t.TempDir()
	for index := 0; index <= session.MaxTurnArtifacts; index++ {
		name := fmt.Sprintf("chart-%d.png", index)
		data := append([]byte("\x89PNG\r\n\x1a\nchart"), byte(index))
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := collectSessionOutputDir(dir); err == nil ||
		!strings.Contains(err.Error(), "too many") {
		t.Fatalf("image count error = %v", err)
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
