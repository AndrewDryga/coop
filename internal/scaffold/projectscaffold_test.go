package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/project"
)

// The scaffolded project.yaml selects filtered access explicitly — the one active key — and
// documents the rest of box:/gate: as comments, so it parses via project.Load as a filtered
// project that asks for nothing a human would have to approve.
func TestScaffoldedProjectParses(t *testing.T) {
	repo := t.TempDir()
	if _, err := WriteProject(repo, nil); err != nil {
		t.Fatal(err)
	}
	p, err := project.Load(repo)
	if err != nil {
		t.Fatalf("scaffolded project.yaml must parse: %v", err)
	}
	if p.Box.Egress != "filtered" || len(p.Box.EgressRules) != 0 || p.Gate != "" {
		t.Errorf("scaffold must load as filtered with no rules and nothing else, got %+v", p)
	}
	data, _ := os.ReadFile(filepath.Join(repo, filepath.FromSlash(project.File)))
	for _, want := range []string{"box:", "env:", "PGHOST:", "  egress: filtered\n", "egress_rules:", "\"offline\"", "gate:"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("scaffold missing %q", want)
		}
	}
	// "none" is an internal spelling; the file a human edits never shows it.
	if strings.Contains(string(data), "none") {
		t.Errorf("scaffold spells offline as none:\n%s", data)
	}
	// Re-running init keeps the file a human may have edited since.
	edited := []byte("box:\n  egress: open\n")
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(project.File)), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if wrote, err := WriteProject(repo, nil); err != nil || wrote {
		t.Fatalf("re-init rewrote an existing project.yaml: wrote=%v err=%v", wrote, err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo, filepath.FromSlash(project.File))); string(data) != string(edited) {
		t.Errorf("re-init changed the project file:\n%s", data)
	}
}
