package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/project"
)

// The scaffolded project.yaml documents the box:/gate: keys as comments, and — being all-commented —
// parses via project.Load as an empty project (no behavior change until a key is uncommented).
func TestScaffoldedProjectParses(t *testing.T) {
	repo := t.TempDir()
	if _, err := WriteProject(repo, nil); err != nil {
		t.Fatal(err)
	}
	p, err := project.Load(repo)
	if err != nil {
		t.Fatalf("scaffolded project.yaml must parse: %v", err)
	}
	if p.Box.Egress != "" || p.Gate != "" {
		t.Errorf("commented scaffold must load empty, got %+v", p)
	}
	data, _ := os.ReadFile(filepath.Join(repo, filepath.FromSlash(project.File)))
	for _, want := range []string{"box:", "egress:", "gate:"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("scaffold missing %q documentation", want)
		}
	}
}
