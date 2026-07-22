package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/project"
)

// ComposeProject is per-workspace: two checkouts with the SAME basename at DIFFERENT paths share an
// image tag but get DISTINCT compose projects (the clone/fork collision fix), and it's stable for a
// path so a workspace's sidecar volumes persist across runs.
func TestComposeProject(t *testing.T) {
	p1 := filepath.Join(t.TempDir(), "myrepo")
	p2 := filepath.Join(t.TempDir(), "myrepo")
	for _, p := range []string{p1, p2} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if ServicesProject(p1) != ServicesProject(p2) {
		t.Fatal("same basename must share the image tag")
	}
	if ComposeProject(p1) == ComposeProject(p2) {
		t.Errorf("same basename at different paths must get DISTINCT compose projects, both %q", ComposeProject(p1))
	}
	if got := ComposeProject(p1); got != ComposeProject(p1) {
		t.Error("compose project must be stable for a path")
	}
	if !strings.HasPrefix(ComposeProject(p1), ServicesProject(p1)+"-") {
		t.Errorf("compose project should be <image-tag>-<hash>, got %q", ComposeProject(p1))
	}
}

func TestServicesProject(t *testing.T) {
	cases := map[string]string{
		"/tmp/My_Repo.Name": "coop-my_reponame",
		"/a/b/agent":        "coop-agent",
		"/x/Project 99!":    "coop-project99",
	}
	for in, want := range cases {
		if got := ServicesProject(in); got != want {
			t.Errorf("ServicesProject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImageForRepo(t *testing.T) {
	dir := t.TempDir()
	if got := ImageForRepo(dir, "coop-box", ""); got != "coop-box" {
		t.Errorf("no .agent/Dockerfile -> %q, want coop-box", got)
	}
	if got := ImageForRepo(dir, "coop-box", "custom"); got != "custom" {
		t.Errorf("override -> %q, want custom", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".agent", "Dockerfile"), []byte("FROM scratch"), 0o644)
	if got := ImageForRepo(dir, "coop-box", ""); got != ServicesProject(dir) {
		t.Errorf(".agent/Dockerfile -> %q, want %q", got, ServicesProject(dir))
	}
}

func TestComposeFile(t *testing.T) {
	dir := t.TempDir()
	if ComposeFile(dir, dir) != "" {
		t.Error("no compose file should yield empty string")
	}
	f := filepath.Join(dir, filepath.FromSlash(ComposeFileRel))
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("services: {}"), 0o644)
	if ComposeFile(dir, dir) != f {
		t.Errorf("ComposeFile = %q, want %q", ComposeFile(dir, dir), f)
	}
	// A root compose.agent.yml is NOT picked up — only .agent/compose.yml is.
	os.Remove(f)
	os.WriteFile(filepath.Join(dir, "compose.agent.yml"), []byte("services: {}"), 0o644)
	if ComposeFile(dir, dir) != "" {
		t.Error("the retired root compose.agent.yml must not be recognized")
	}
	os.WriteFile(f, []byte("services: {}"), 0o644) // restore .agent/compose.yml for the zero-byte check
	// A zero-byte file counts as none — it declares no services, so `compose up` would only error.
	os.WriteFile(f, nil, 0o644)
	if ComposeFile(dir, dir) != "" {
		t.Error("an empty compose file should count as no compose file")
	}

	// The config/runtime split: the relative PATH comes from the POLICY repo, the FILE from the
	// workspace at that path — so a fork uses the parent's committed compose LOCATION but its own file.
	policy := t.TempDir()
	os.MkdirAll(filepath.Join(policy, ".agent"), 0o755)
	os.WriteFile(filepath.Join(policy, filepath.FromSlash(project.File)), []byte("box:\n  compose: build/svc.yml\n"), 0o644)
	ws := t.TempDir()
	wf := filepath.Join(ws, "build", "svc.yml")
	os.MkdirAll(filepath.Dir(wf), 0o755)
	os.WriteFile(wf, []byte("services: {}"), 0o644)
	if got := ComposeFile(ws, policy); got != wf {
		t.Errorf("path must come from policy repo, file from workspace: got %q, want %q", got, wf)
	}
}
