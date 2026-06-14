package box

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// find returns the mount targeting target, or nil.
func find(mounts []Mount, target string) *Mount {
	for i := range mounts {
		if mounts[i].Target == target {
			return &mounts[i]
		}
	}
	return nil
}

// fixture builds a repo tree exercising every shadowing rule and returns its path.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "SECRET=hunter2")            // secret file at root
	write(".env.example", "KEY=placeholder")   // allow-listed template
	write("config/prod.tfvars", `x="hunter2"`) // secret nested in a normal dir
	write("secrets/api-token", "tok")          // secret dir (contents must be hidden whole)
	write(".ssh/id_rsa", "key")                // secret dir by exact name
	write("src/app.js", "console.log(1)")      // ordinary source
	write(".git/config", "[core]")             // must be skipped, not shadowed
	write("deploy.pem", "----")                // secret file by extension
	return root
}

func TestComputeMounts(t *testing.T) {
	repo := fixture(t)
	mounts, err := ComputeMounts(repo, "/workspace")
	if err != nil {
		t.Fatal(err)
	}

	// The first mount is always the repo bind.
	if mounts[0].Kind != Bind || mounts[0].Source != repo || mounts[0].Target != "/workspace" {
		t.Fatalf("first mount is not the repo bind: %+v", mounts[0])
	}

	// .env is shadowed by a read-only decoy.
	if m := find(mounts, "/workspace/.env"); m == nil || m.Kind != Decoy || !m.RO {
		t.Errorf(".env not shadowed by a read-only decoy: %+v", m)
	}
	// A secret nested in an ordinary dir is found and shadowed.
	if m := find(mounts, "/workspace/config/prod.tfvars"); m == nil || m.Kind != Decoy {
		t.Errorf("config/prod.tfvars not shadowed: %+v", m)
	}
	// deploy.pem shadowed by extension.
	if m := find(mounts, "/workspace/deploy.pem"); m == nil || m.Kind != Decoy {
		t.Errorf("deploy.pem not shadowed: %+v", m)
	}
	// Secret directories become a tmpfs.
	if m := find(mounts, "/workspace/secrets"); m == nil || m.Kind != Tmpfs {
		t.Errorf("secrets/ not shadowed by tmpfs: %+v", m)
	}
	if m := find(mounts, "/workspace/.ssh"); m == nil || m.Kind != Tmpfs {
		t.Errorf(".ssh/ not shadowed by tmpfs: %+v", m)
	}

	// The allow-listed template and ordinary source must remain visible.
	if find(mounts, "/workspace/.env.example") != nil {
		t.Error(".env.example must not be shadowed (allow-listed)")
	}
	if find(mounts, "/workspace/src/app.js") != nil {
		t.Error("src/app.js must not be shadowed")
	}

	// A shadowed dir hides its contents whole — no per-file mount inside it.
	if find(mounts, "/workspace/secrets/api-token") != nil {
		t.Error("contents of secrets/ must not be enumerated (dir is pruned)")
	}
	if find(mounts, "/workspace/.ssh/id_rsa") != nil {
		t.Error("contents of .ssh/ must not be enumerated (dir is pruned)")
	}

	// .git is skipped entirely: not shadowed, not descended.
	if find(mounts, "/workspace/.git") != nil || find(mounts, "/workspace/.git/config") != nil {
		t.Error(".git must be skipped, neither shadowed nor descended")
	}

	// Exactly the five secrets above are shadowed.
	if got := ShadowCount(mounts); got != 5 {
		t.Errorf("ShadowCount = %d, want 5", got)
	}
}

func TestRenderMounts(t *testing.T) {
	mounts := []Mount{
		{Kind: Bind, Source: "/repo", Target: "/workspace"},
		{Kind: Decoy, Target: "/workspace/.env", RO: true},
		{Kind: Tmpfs, Target: "/workspace/secrets"},
	}
	got := RenderMounts(mounts, "/tmp/decoy")
	want := []string{
		"-v", "/repo:/workspace",
		"-v", "/tmp/decoy:/workspace/.env:ro",
		"--tmpfs", "/workspace/secrets",
	}
	if !slices.Equal(got, want) {
		t.Errorf("RenderMounts = %v, want %v", got, want)
	}
}

func TestMatchesAny(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{".env", true}, {".env.local", true}, {"id_ed25519", true}, {"id_ed25519.pub", true},
		{"prod.tfvars", true}, {"secrets", true}, {".ssh", true},
		{"app.js", false}, {"README.md", false}, {"env", false}, {"app.env", false},
	}
	for _, c := range cases {
		if got := matchesAny(c.name, SecretGlobs); got != c.want {
			t.Errorf("matchesAny(%q, secrets) = %v, want %v", c.name, got, c.want)
		}
	}
}
