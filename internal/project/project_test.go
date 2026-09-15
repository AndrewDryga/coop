package project

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

func TestLoadBoundsRepositoryInput(t *testing.T) {
	repo := writeProject(t, "#"+strings.Repeat("x", maxProjectBytes))
	if _, err := Load(repo); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized config: %v", err)
	}
}

func TestProjectCaptureRejectsReplacedSources(t *testing.T) {
	for _, replacement := range []string{"parent", "file", "symlink", "fifo"} {
		t.Run(replacement, func(t *testing.T) {
			repo := writeProject(t, "box:\n  egress: offline\n")
			path := filepath.Join(repo, File)
			parent, err := os.Lstat(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if replacement == "parent" {
				if err := os.Rename(filepath.Dir(path), filepath.Join(repo, "original")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			switch replacement {
			case "parent", "file":
				err = os.WriteFile(path, []byte("box:\n  egress: open\n"), 0o600)
			case "symlink":
				err = os.Symlink(path+".original", path)
			case "fifo":
				err = syscall.Mkfifo(path, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readProjectFile(path, parent, before); err == nil {
				t.Fatal("captured a replaced source")
			}
		})
	}
}

func TestLoadRejectsPresentUnsafeEntries(t *testing.T) {
	assertPolicyError := func(t *testing.T, repo string) {
		t.Helper()
		if _, err := Load(repo); err == nil || !strings.Contains(err.Error(), File) {
			t.Fatalf("Load = %v, want an error naming %s", err, File)
		}
	}

	t.Run("symlink", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "project.yaml")
		if err := os.WriteFile(target, []byte("serve: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(repo, File)); err != nil {
			t.Fatal(err)
		}
		assertPolicyError(t, repo)
		assertSymlinkRemedy(t, repo, "Coop requires a regular project file.")
	})

	t.Run("dangling symlink", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(repo, "missing"), filepath.Join(repo, File)); err != nil {
			t.Fatal(err)
		}
		assertPolicyError(t, repo)
	})

	t.Run("symlinked agent directory", func(t *testing.T) {
		repo := t.TempDir()
		target := t.TempDir()
		if err := os.WriteFile(filepath.Join(target, "project.yaml"), []byte("serve: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(repo, ".agent")); err != nil {
			t.Fatal(err)
		}
		assertPolicyError(t, repo)
		assertSymlinkRemedy(t, repo, "Coop reads project config without following links.")
	})

	t.Run("directory", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repo, File), 0o755); err != nil {
			t.Fatal(err)
		}
		assertPolicyError(t, repo)
	})

	t.Run("unreadable", func(t *testing.T) {
		repo := writeProject(t, "serve: {}\n")
		path := filepath.Join(repo, File)
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if _, err := Load(repo); err == nil {
			if os.Geteuid() == 0 {
				t.Skip("root can read mode-000 files")
			}
			t.Fatalf("Load unreadable %s = nil error", File)
		} else if !strings.Contains(err.Error(), File) {
			t.Fatalf("Load error = %v, want it to name %s", err, File)
		}
	})
}

// TestLoadParse: subprojects + serve ports both parse.
func TestLoadParse(t *testing.T) {
	repo := writeProject(t, "subprojects:\n  - runner\n  - packs\nserve:\n  ports:\n    - 5173\n    - 3000\ngate_sources:\n  - run\n  - tools/internal/devtool/gates.go\n")
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
	if !slices.Equal(p.GateSources, []string{"run", "tools/internal/devtool/gates.go"}) {
		t.Errorf("gate sources = %v", p.GateSources)
	}
}

// TestLoadInvalid: a typo surfaces as an error, not a silent no-op.
func TestLoadInvalid(t *testing.T) {
	cases := map[string]string{
		"bad port":             "serve:\n  ports:\n    - 70000\n",
		"zero port":            "serve:\n  ports:\n    - 0\n",
		"bad yaml":             "serve: [\n",
		"second document":      "---\n---\nbox:\n  egress: offline\n",
		"absolute sub":         "subprojects:\n  - /etc\n",
		"escaping sub":         "subprojects:\n  - ../evil\n",
		"empty gate source":    "gate_sources:\n  - \"\"\n",
		"absolute gate source": "gate_sources:\n  - /tmp/check.sh\n",
		"escaping gate source": "gate_sources:\n  - ../check.sh\n",
		// KnownFields: an unknown key (a typo'd `subproject:`) errors instead of silently doing nothing.
		"unknown key":                    "subproject:\n  - runner\n",
		"unknown box key":                "box:\n  egres: none\n",
		"bad egress":                     "box:\n  egress: full\n",
		"retired egress spelling":        "box:\n  egress: none\n", // the file says offline; none is internal only
		"bad pids":                       "box:\n  pids: lots\n",
		"negative pids":                  "box:\n  pids: \"-5\"\n",
		"no_new_privileges is not a key": "box:\n  no_new_privileges: false\n",
		"empty env key":                  "box:\n  env:\n    \"\": value\n",
		"bad env key":                    "box:\n  env:\n    1HOST: db\n",
		"reserved env key":               "box:\n  env:\n    COOP_BOX: fake\n",
		"multiline env value":            "box:\n  env:\n    VALUE: |\n      one\n      two\n",
		"escaping review compose":        "review:\n  compose: ../compose.yml\n",
		"reserved review env":            "review:\n  env:\n    COOP_BOX: fake\n",
		"multiline review env":           "review:\n  env:\n    VALUE: |\n      one\n      two\n",
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
	p, err := Load(writeProject(t, "box:\n  env:\n    PGHOST: db\n    PGPORT: \"5432\"\n  egress: offline\n  auto_up: false\n  memory: 4g\n  cpus: \"2\"\n  pids: 2048\nreview:\n  compose: dev/review-compose.yml\n  env:\n    CI: \"1\"\n    DATABASE_URL: postgres://postgres@db/test\ngate: make check\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.Box.Egress != "none" || p.Box.Memory != "4g" || p.Box.CPUs != "2" || p.Box.Pids != "2048" {
		t.Errorf("box = %+v", p.Box)
	}
	if p.Box.AutoUp == nil || *p.Box.AutoUp {
		t.Errorf("auto_up should be explicitly false, got %v", p.Box.AutoUp)
	}
	if p.Box.Env["PGHOST"] != "db" || p.Box.Env["PGPORT"] != "5432" {
		t.Errorf("box env = %v", p.Box.Env)
	}
	if p.Box.Network != nil {
		t.Errorf("network was absent — must stay nil (absent ≠ false), got %v", *p.Box.Network)
	}
	if p.Gate != "make check" || len(p.GateSources) != 0 {
		t.Errorf("gate = %q", p.Gate)
	}
	if p.Review.Compose != "dev/review-compose.yml" || p.Review.Env["CI"] != "1" {
		t.Errorf("review = %+v", p.Review)
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

// assertSymlinkRemedy checks that a refused symlink is named as one, with what to do about it —
// a dotfile-managed .agent or project.yaml is a common setup, and "must be a directory" (which it
// is) or "must be a regular file" sent people looking in the wrong place.
func assertSymlinkRemedy(t *testing.T, repo, requirement string) {
	t.Helper()
	_, err := Load(repo)
	if err == nil || !strings.Contains(err.Error(), "is a symbolic link") || !strings.Contains(err.Error(), requirement) {
		t.Fatalf("Load error = %v; want the link named and the requirement %q", err, requirement)
	}
}
