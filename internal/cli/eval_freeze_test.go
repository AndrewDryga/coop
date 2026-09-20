package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/eval"
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

// A loop suite freezes the loop.yaml it runs under: the suite's own loop_config resolves under the
// suite dir; a --loop-config override resolves against the current directory.
func TestFreezeConfigurationCapturesTheLoopRecipe(t *testing.T) {
	repo := evalTestRepo(t)
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	dir := t.TempDir()
	must := func(rel, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("loop.yaml", "work:\n  round_limit: 3\n")
	must("fixtures/app/.keep", "")
	must("queues/repo/.keep", "")
	must("verifiers/repo/.keep", "")
	must("suite.yaml", "version: 1\nname: loops\nrunner: loop\nloop_config: ./loop.yaml\ncases:\n  - id: repo\n    fixture: ./fixtures/app\n    tasks: ./queues/repo\n    verifier: ./verifiers/repo\n    timeout: 50m\n")

	suite, err := eval.Load(filepath.Join(dir, "suite.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	loopPath := filepath.Join(suite.Dir, "loop.yaml")
	frozen, err := a.freezeConfiguration(eval.Configuration{Kind: eval.ConfigTarget, Label: "codex"}, suite, loopPath, a.evalBuildIdentity())
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if string(frozen.LoopConfig) != "work:\n  round_limit: 3\n" {
		t.Errorf("loop recipe not frozen: %q", frozen.LoopConfig)
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
