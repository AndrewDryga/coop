package project

import (
	"os"
	"path/filepath"
	"testing"
)

func writeProject(t *testing.T, body string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, File), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// TestLoadMissing: no .agent/project.yaml is the common single-repo case — empty, no error.
func TestLoadMissing(t *testing.T) {
	p, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if len(p.Subprojects) != 0 || len(p.Serve.Ports) != 0 {
		t.Errorf("missing file must yield empty project, got %+v", p)
	}
}

// TestLoadParse: subprojects + serve ports both parse.
func TestLoadParse(t *testing.T) {
	repo := writeProject(t, "subprojects:\n  - runner\n  - packs\nserve:\n  ports:\n    - 5173\n    - 3000\n")
	p, err := Load(repo)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(p.Subprojects) != 2 || p.Subprojects[0] != "runner" || p.Subprojects[1] != "packs" {
		t.Errorf("subprojects = %v", p.Subprojects)
	}
	if len(p.Serve.Ports) != 2 || p.Serve.Ports[0] != 5173 {
		t.Errorf("ports = %v", p.Serve.Ports)
	}
}

// TestLoadInvalid: a typo surfaces as an error, not a silent no-op.
func TestLoadInvalid(t *testing.T) {
	cases := map[string]string{
		"bad port":     "serve:\n  ports:\n    - 70000\n",
		"zero port":    "serve:\n  ports:\n    - 0\n",
		"bad yaml":     "serve: [\n",
		"absolute sub": "subprojects:\n  - /etc\n",
		"escaping sub": "subprojects:\n  - ../evil\n",
	}
	for name, body := range cases {
		if _, err := Load(writeProject(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestTaskDirs: single repo → just .agent/tasks; monorepo → each subproject's queue (+ root's if it
// has one). Falls back to .agent/tasks when there's no project.yaml.
func TestTaskDirs(t *testing.T) {
	// No project.yaml → the plain default.
	got, err := TaskDirs(t.TempDir())
	if err != nil || len(got) != 1 || got[0] != filepath.Join(".agent", "tasks") {
		t.Fatalf("no project.yaml → %v, %v; want [.agent/tasks]", got, err)
	}

	// Monorepo without a root queue → only the members'.
	repo := writeProject(t, "subprojects:\n  - runner\n  - packs\n")
	got, err = TaskDirs(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join("runner", ".agent", "tasks"), filepath.Join("packs", ".agent", "tasks")}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("monorepo dirs = %v, want %v", got, want)
	}

	// Root that ALSO has its own queue → it's included first.
	if err := os.MkdirAll(filepath.Join(repo, ".agent", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, _ = TaskDirs(repo)
	if len(got) != 3 || got[0] != filepath.Join(".agent", "tasks") {
		t.Errorf("with a root queue, dirs = %v, want root first + 2 members", got)
	}
}

// TestHostPort: deterministic per (repo, port), distinct across repos and ports, within the window.
func TestHostPort(t *testing.T) {
	if a1, a2 := HostPort("/a", 5173), HostPort("/a", 5173); a1 != a2 {
		t.Errorf("HostPort must be deterministic for the same repo+port: %d vs %d", a1, a2)
	}
	if HostPort("/a", 5173) == HostPort("/b", 5173) {
		t.Error("different repos should map to different host ports")
	}
	if HostPort("/a", 5173) == HostPort("/a", 3000) {
		t.Error("different container ports should map to different host ports")
	}
	if p := HostPort("/a", 5173); p < hostPortBase || p >= hostPortBase+hostPortSpan {
		t.Errorf("host port %d outside [%d,%d)", p, hostPortBase, hostPortBase+hostPortSpan)
	}
}
