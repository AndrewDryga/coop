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
		// KnownFields: an unknown key (a typo'd `subproject:`) errors instead of silently doing nothing.
		"unknown key":                    "subproject:\n  - runner\n",
		"unknown box key":                "box:\n  egres: none\n",
		"bad egress":                     "box:\n  egress: full\n",
		"bad pids":                       "box:\n  pids: lots\n",
		"negative pids":                  "box:\n  pids: \"-5\"\n",
		"no_new_privileges is not a key": "box:\n  no_new_privileges: false\n",
	}
	for name, body := range cases {
		if _, err := Load(writeProject(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestLoadBoxGate: the committed box policy + merge gate parse; pointer booleans keep absent ≠ false;
// an all-comments file (the scaffolded template) is valid and empty.
func TestLoadBoxGate(t *testing.T) {
	p, err := Load(writeProject(t, "box:\n  egress: none\n  auto_up: false\n  memory: 4g\n  cpus: \"2\"\n  pids: 2048\ngate: make check\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.Box.Egress != "none" || p.Box.Memory != "4g" || p.Box.CPUs != "2" || p.Box.Pids != "2048" {
		t.Errorf("box = %+v", p.Box)
	}
	if p.Box.AutoUp == nil || *p.Box.AutoUp {
		t.Errorf("auto_up should be explicitly false, got %v", p.Box.AutoUp)
	}
	if p.Box.Network != nil {
		t.Errorf("network was absent — must stay nil (absent ≠ false), got %v", *p.Box.Network)
	}
	if p.Gate != "make check" {
		t.Errorf("gate = %q", p.Gate)
	}
	// An all-comments file (what coop init scaffolds) parses as empty, not an EOF error.
	if p, err := Load(writeProject(t, "# only comments\n#box:\n#  egress: none\n")); err != nil || p.Box.Egress != "" {
		t.Errorf("all-comments file must load empty, got %+v, %v", p, err)
	}
	// unlimited / 0 pids are accepted spellings for "no cap".
	if _, err := Load(writeProject(t, "box:\n  pids: unlimited\n")); err != nil {
		t.Errorf("pids: unlimited must be accepted: %v", err)
	}
}

// TestBoxDockerfileComposePaths: box.dockerfile / box.compose resolve to their configured value,
// else the .agent/ defaults; an escaping or absolute path is rejected at Load.
func TestBoxDockerfileComposePaths(t *testing.T) {
	// Absent project.yaml (and unset keys) → the defaults.
	empty := t.TempDir()
	if got := DockerfilePath(empty); got != DefaultDockerfile {
		t.Errorf("default dockerfile = %q, want %q", got, DefaultDockerfile)
	}
	if got := ComposePath(empty); got != DefaultCompose {
		t.Errorf("default compose = %q, want %q", got, DefaultCompose)
	}
	// Configured overrides win (and are cleaned).
	repo := writeProject(t, "box:\n  dockerfile: build/box.Dockerfile\n  compose: ./docker-compose.yml\n")
	if got := DockerfilePath(repo); got != "build/box.Dockerfile" {
		t.Errorf("override dockerfile = %q, want build/box.Dockerfile", got)
	}
	if got := ComposePath(repo); got != "docker-compose.yml" {
		t.Errorf("override compose = %q, want docker-compose.yml", got)
	}
	// A path that escapes or leaves the repo is rejected at Load (tighten-only, like subprojects).
	for _, bad := range []string{"box:\n  dockerfile: ../evil\n", "box:\n  compose: /etc/passwd\n"} {
		if _, err := Load(writeProject(t, bad)); err == nil {
			t.Errorf("expected an error for out-of-repo box path:\n%s", bad)
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
