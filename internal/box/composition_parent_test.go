package box

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// A filtered run points composition at its execution's private artifact
// directory. Ordinary runs pass "" and keep the system temp dir, including
// their existing cleanup — this proves the seam changed neither behavior.
func TestRunComposesUnderExplicitParentWithoutChangingOrdinaryCleanup(t *testing.T) {
	parent := t.TempDir()
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = parent
	originalWrite := artifacts.writeFile
	var generated []string
	artifacts.writeFile = func(root, content string) (string, error) {
		if root != parent {
			t.Fatal("run lost explicit composition parent")
		}
		path, err := originalWrite(root, content)
		if err == nil {
			generated = append(generated, path)
		}
		return path, err
	}
	repo := t.TempDir()
	writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), "box:\n  env:\n    FIXTURE_ENV: synthetic\n")
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	code, err := runWithCompositionArtifacts(cfg, recorderRuntime(t, recorder), RunSpec{
		Image: "fixture", Repo: repo, Cmd: []string{"true"}, Agent: "claude", Homes: true, Batch: true, Quiet: true,
	}, artifacts)
	if err != nil || code != 0 || len(generated) < 2 {
		t.Fatalf("run composition: code=%d generated=%d err=%v", code, len(generated), err)
	}
	for _, path := range generated {
		if filepath.Dir(path) != parent {
			t.Fatalf("generated artifact escaped its parent: %s", path)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("ordinary run changed its generated-artifact cleanup", err)
	}
}
