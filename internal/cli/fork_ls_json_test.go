package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
)

// coop fork ls --json reports the root workspace (even with no forks) and its per-port serve URLs,
// each derived from project.HostPort of the WORKSPACE path — so host tooling discovers URLs without
// reproducing the port hash.
func TestForkLsJSON(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("serve:\n  ports: [4000]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	out := captureStdout(t, func() {
		if code, err := a.forkLs([]string{"--json"}); code != 0 || err != nil {
			t.Fatalf("fork ls --json: (%d, %v)", code, err)
		}
	})
	var got struct {
		Workspaces []struct {
			Name  string            `json:"name"`
			Path  string            `json:"path"`
			Serve map[string]string `json:"serve"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if len(got.Workspaces) != 1 || got.Workspaces[0].Name != "root" {
		t.Fatalf("want a single root workspace, got %+v", got.Workspaces)
	}
	want := fmt.Sprintf("http://localhost:%d", project.HostPort(repo, 4000))
	if got.Workspaces[0].Serve["4000"] != want {
		t.Errorf("serve URL for 4000 = %q, want %q", got.Workspaces[0].Serve["4000"], want)
	}
}
