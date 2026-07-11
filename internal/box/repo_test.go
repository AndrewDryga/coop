package box

import (
	"os"
	"path/filepath"
	"testing"
)

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
		t.Errorf("no Dockerfile.agent -> %q, want coop-box", got)
	}
	if got := ImageForRepo(dir, "coop-box", "custom"); got != "custom" {
		t.Errorf("override -> %q, want custom", got)
	}
	os.WriteFile(filepath.Join(dir, "Dockerfile.agent"), []byte("FROM scratch"), 0o644)
	if got := ImageForRepo(dir, "coop-box", ""); got != ServicesProject(dir) {
		t.Errorf("Dockerfile.agent -> %q, want %q", got, ServicesProject(dir))
	}
}

func TestComposeFile(t *testing.T) {
	dir := t.TempDir()
	if ComposeFile(dir) != "" {
		t.Error("no compose file should yield empty string")
	}
	f := filepath.Join(dir, filepath.FromSlash(ComposeFileRel))
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("services: {}"), 0o644)
	if ComposeFile(dir) != f {
		t.Errorf("ComposeFile = %q, want %q", ComposeFile(dir), f)
	}
	// A root compose.agent.yml is NOT picked up — only .agent/compose.yml is.
	os.Remove(f)
	os.WriteFile(filepath.Join(dir, "compose.agent.yml"), []byte("services: {}"), 0o644)
	if ComposeFile(dir) != "" {
		t.Error("the retired root compose.agent.yml must not be recognized")
	}
	os.WriteFile(f, []byte("services: {}"), 0o644) // restore .agent/compose.yml for the zero-byte check
	// A zero-byte file counts as none — it declares no services, so `compose up` would only error.
	os.WriteFile(f, nil, 0o644)
	if ComposeFile(dir) != "" {
		t.Error("an empty compose file should count as no compose file")
	}
}
