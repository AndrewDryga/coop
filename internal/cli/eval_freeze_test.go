package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func evalTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil { // box.ResolveRepo needs a git dir
		t.Fatal(err)
	}
	return repo
}

// Freezing a preset captures its actual content — the preset.yaml AND every prompt it loads — so
// editing either between runs changes its configuration fingerprint.
func TestFreezePresetContentTracksManifestAndPromptEdits(t *testing.T) {
	repo := evalTestRepo(t)
	dir := writePresetFile(t, repo, "demo", "lead:\n  agent: [codex:gpt-5.6/high]\n  prompt: lead.md\n")
	if err := os.WriteFile(filepath.Join(dir, "lead.md"), []byte("Be careful.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}

	first, err := a.freezePresetContent("demo")
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	// Editing only the PROMPT file (not preset.yaml) must still change the frozen content.
	if err := os.WriteFile(filepath.Join(dir, "lead.md"), []byte("Be reckless.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	afterPrompt, err := a.freezePresetContent("demo")
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if string(first) == string(afterPrompt) {
		t.Error("editing the lead prompt did not change the frozen content")
	}
	// Editing preset.yaml changes it too.
	writePresetFile(t, repo, "demo", "lead:\n  agent: [claude:opus/high]\n  prompt: lead.md\n")
	afterManifest, err := a.freezePresetContent("demo")
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if string(afterPrompt) == string(afterManifest) {
		t.Error("editing preset.yaml did not change the frozen content")
	}
}

func TestEvalBuildIdentityDigestsTheExecutable(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	id := a.evalBuildIdentity()
	// The test binary exists and is readable, so a digest is always computed; the digest is what
	// keeps two builds of one version distinct.
	if id.Digest == "" {
		t.Error("build identity has no executable digest")
	}
}
