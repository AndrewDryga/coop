package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/preset"
)

// One grammar, one error set: a malformed target reads the SAME on every surface — the CLI
// positional, a preset's lead.agent, a preset role's agent:, and a fleet entry — because they
// all funnel through agents.ParseTarget. Each surface's error must carry the parser's own
// message verbatim, so a typo diagnoses identically wherever it was written.
func TestTargetErrorsAgreeAcrossSurfaces(t *testing.T) {
	cases := []struct{ name, target string }{
		{"unknown provider", "gpt4"},
		{"empty model", "claude:"},
		{"traversal account", "claude:opus@../x"},
		{"double at", "claude:opus@work@x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, perr := agents.ParseTarget(c.target)
			if perr == nil {
				t.Fatalf("ParseTarget(%q) = nil, the suite needs a malformed target", c.target)
			}
			want := perr.Error()

			// CLI surface: the loop's positional target.
			if _, _, _, _, err := parseLoopArgs([]string{c.target}, false); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("loop positional: err = %v, want it to carry %q", err, want)
			}

			// Preset lead.agent.
			if _, err := loadTempPreset(t, fmt.Sprintf("lead: {agent: %q}\n", c.target)); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("preset lead.agent: err = %v, want it to carry %q", err, want)
			}

			// Preset role agent: (roles reject @accounts before parsing errors surface only for
			// account-shaped cases — those get the role's own message; the rest must match).
			if !strings.Contains(c.target, "@") {
				yaml := fmt.Sprintf("lead: {agent: claude}\nroles: {r: {mode: consult, agent: %q}}", c.target)
				if _, err := loadTempPreset(t, yaml); err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("preset role agent: err = %v, want it to carry %q", err, want)
				}
			}

			// Fleet entry agent:.
			fleet := fmt.Sprintf("forks:\n  a:\n    tasks: .agent/tasks\n    agent: %q\n", c.target)
			if _, err := parseFleetYAML(fleet); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("fleet agent: err = %v, want it to carry %q", err, want)
			}
		})
	}
}

// loadTempPreset writes a one-off preset.yaml in a temp repo and loads it.
func loadTempPreset(t *testing.T, yaml string) (*preset.Preset, error) {
	t.Helper()
	repo := t.TempDir()
	dir := filepath.Join(repo, ".agent", "presets", "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return preset.Load(repo, "", "p")
}
