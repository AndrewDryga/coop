package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// gateFor: an explicit COOP_GATE (env/conf) wins; otherwise the committed .agent/project.yaml gate:.
func TestGateFor(t *testing.T) {
	repoWith := func(t *testing.T, body string) string {
		repo := t.TempDir()
		os.MkdirAll(filepath.Join(repo, ".agent"), 0o755)
		if body != "" {
			os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(body), 0o644)
		}
		return repo
	}

	t.Run("project gate used when COOP_GATE unset", func(t *testing.T) {
		a := &app{cfg: config.Load()}
		if g := a.gateFor(repoWith(t, "gate: make check\n")); len(g) != 2 || g[0] != "make" || g[1] != "check" {
			t.Errorf("project gate: = %v, want [make check]", g)
		}
	})
	t.Run("explicit COOP_GATE beats the file", func(t *testing.T) {
		t.Setenv("COOP_GATE", "go test ./...")
		a := &app{cfg: config.Load()}
		if g := a.gateFor(repoWith(t, "gate: make check\n")); len(g) != 3 || g[0] != "go" {
			t.Errorf("explicit COOP_GATE must win, got %v", g)
		}
	})
	t.Run("neither set → no gate", func(t *testing.T) {
		if g := (&app{cfg: config.Load()}).gateFor(repoWith(t, "")); len(g) != 0 {
			t.Errorf("no gate anywhere → empty, got %v", g)
		}
	})
}
