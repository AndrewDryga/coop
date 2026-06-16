package box

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestAuthedAgents(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir, Agents: []string{"claude", "codex", "gemini"}}

	// codex authed via its credential file; gemini via an env-file API key; claude
	// has neither → not authed. A commented or empty key must not count.
	os.MkdirAll(filepath.Join(dir, "codex"), 0o755)
	os.WriteFile(filepath.Join(dir, "codex", "auth.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "env"), []byte("# OPENAI_API_KEY=ignored\nGEMINI_API_KEY=real\nEMPTY=\n"), 0o644)

	if got := AuthedAgents(cfg); !slices.Equal(got, []string{"codex", "gemini"}) {
		t.Errorf("AuthedAgents = %v, want [codex gemini]", got)
	}
	if got := authedPeers(cfg, "codex"); !slices.Equal(got, []string{"gemini"}) {
		t.Errorf("authedPeers(codex) = %v, want [gemini]", got)
	}

	// With only the lead authed there are no peers → nothing to consult.
	solo := &config.Config{ConfigDir: dir, Agents: []string{"codex"}}
	if got := authedPeers(solo, "codex"); len(got) != 0 {
		t.Errorf("authedPeers with only the lead authed = %v, want none", got)
	}
}
