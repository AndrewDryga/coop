package sessionsvc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
)

func TestCollectSessionOutputDirAcceptsImagesAndRejectsUnsafeFiles(t *testing.T) {
	output, _, err := prepareSessionOutputDirAtRoot(t.TempDir(), "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	defer removeSessionOutputDir(output)
	dir := output.path
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
	artifacts, err := collectSessionOutputDir(output)
	if err != nil || len(artifacts) != 1 || artifacts[0].Name != "load.png" || string(artifacts[0].Data) != string(data) {
		t.Fatalf("artifacts = %+v err=%v", artifacts, err)
	}
	if err := os.Symlink(filepath.Join(dir, "load.png"), filepath.Join(dir, "leak.png")); err != nil {
		t.Fatal(err)
	}
	if _, err := collectSessionOutputDir(output); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("symlink output error = %v", err)
	}
}

func TestCollectSessionOutputDirCountsOnlyImageCandidates(t *testing.T) {
	output, _, err := prepareSessionOutputDirAtRoot(t.TempDir(), "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	defer removeSessionOutputDir(output)
	dir := output.path
	for _, name := range []string{"source.csv", "notes.txt", "chart.json", "scratch.log", "table.tsv"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("scratch"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("\x89PNG\r\n\x1a\nchart")
	if err := os.WriteFile(filepath.Join(dir, "load.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := collectSessionOutputDir(output)
	if err != nil || len(artifacts) != 1 || artifacts[0].Name != "load.png" {
		t.Fatalf("artifacts = %+v err=%v", artifacts, err)
	}
}

func TestCollectSessionOutputDirStillBoundsImageCandidates(t *testing.T) {
	output, _, err := prepareSessionOutputDirAtRoot(t.TempDir(), "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	defer removeSessionOutputDir(output)
	dir := output.path
	for index := 0; index <= session.MaxTurnArtifacts; index++ {
		name := fmt.Sprintf("chart-%d.png", index)
		data := append([]byte("\x89PNG\r\n\x1a\nchart"), byte(index))
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := collectSessionOutputDir(output); err == nil ||
		!strings.Contains(err.Error(), "too many") {
		t.Fatalf("image count error = %v", err)
	}
}

func TestCollectSessionOutputDirBoundsAllDirectoryEntries(t *testing.T) {
	output, _, err := prepareSessionOutputDirAtRoot(t.TempDir(), "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	defer removeSessionOutputDir(output)
	for index := 0; index <= sessionOutputDirEntryLimit; index++ {
		name := fmt.Sprintf("scratch-%03d.txt", index)
		if err := os.WriteFile(filepath.Join(output.path, name), []byte("scratch"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := collectSessionOutputDir(output); err == nil || !strings.Contains(err.Error(), "too many entries") {
		t.Fatalf("directory entry bound error = %v", err)
	}
}

func TestCollectSessionOutputDirDoesNotBlockWhenValidatedFileBecomesFIFO(t *testing.T) {
	output, _, err := prepareSessionOutputDirAtRoot(t.TempDir(), "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	defer removeSessionOutputDir(output)
	path := filepath.Join(output.path, "chart.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nchart"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := beforeSessionOutputArtifactOpen
	beforeSessionOutputArtifactOpen = func(_ *sessionOutputDirectory, name string) {
		if name != "chart.png" {
			return
		}
		if err := os.Remove(path); err != nil {
			t.Errorf("remove validated artifact: %v", err)
			return
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Errorf("replace artifact with FIFO: %v", err)
		}
	}
	t.Cleanup(func() { beforeSessionOutputArtifactOpen = previous })

	done := make(chan error, 1)
	go func() {
		_, err := collectSessionOutputDir(output)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("FIFO replacement error = %v, want unsafe refusal", err)
		}
	case <-time.After(2 * time.Second):
		// Unblock a regressed blocking reader so the test process can cleanly finish.
		writer, _ := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if writer != nil {
			_ = writer.Close()
		}
		t.Fatal("collector blocked opening a repository-controlled FIFO")
	}
}

func TestCollectSessionOutputDirRejectsAReplacedDirectoryWithoutReadingHostFiles(t *testing.T) {
	root := t.TempDir()
	output, _, err := prepareSessionOutputDirAtRoot(root, "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	defer removeSessionOutputDir(output)
	original := output.path + "-moved"
	if err := os.Rename(output.path, original); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	secret := []byte("\x89PNG\r\n\x1a\nhost-private")
	if err := os.WriteFile(filepath.Join(outside, "private.png"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, output.path); err != nil {
		t.Fatal(err)
	}
	artifacts, err := collectSessionOutputDir(output)
	if err == nil || !strings.Contains(err.Error(), "directory was replaced") || len(artifacts) != 0 {
		t.Fatalf("replaced output directory = %+v, %v; want refusal without host artifacts", artifacts, err)
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

func TestPrepareSessionOutputRootCreatesOnlyAnExactDirectory(t *testing.T) {
	workspace := t.TempDir()
	root, err := prepareSessionOutputRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(workspace, sessionOutputRoot) {
		t.Fatalf("output root = %q, want exact workspace child", root)
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("output root info = %+v, %v; want a real directory", info, err)
	}

	unsafeWorkspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(unsafeWorkspace, sessionOutputRoot)); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSessionOutputRoot(unsafeWorkspace); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("symlink output root error = %v, want unsafe refusal", err)
	}
}

func TestRemovingTurnOutputKeepsWarmProcessMountRoot(t *testing.T) {
	root := t.TempDir()
	dir, _, err := prepareSessionOutputDirAtRoot(root, "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := removeSessionOutputDir(dir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		t.Fatalf("turn cleanup removed the warm process mount root: %+v, %v", info, err)
	}
}

func TestPrivateSessionCleanupRemovesCredentialsAndOutput(t *testing.T) {
	stateRoot := t.TempDir()
	sessionID := "remote_11111111111111111111111111111111"
	for _, kind := range []string{"acp", "output"} {
		path := filepath.Join(stateRoot, kind, sessionID)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "owned"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := removePrivateSessionState(stateRoot, sessionID); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"acp", "output"} {
		if _, err := os.Lstat(filepath.Join(stateRoot, kind, sessionID)); !os.IsNotExist(err) {
			t.Fatalf("private %s state survived cleanup: %v", kind, err)
		}
	}
}
