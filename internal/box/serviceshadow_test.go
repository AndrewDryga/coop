package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A sidecar bind of the repo must not hand the sidecar the raw files the box only sees as
// decoys: the override shadows every hidden descendant of a directory bind, replaces a bind whose
// source is itself a secret (through a symlink alias too), and leaves public files, named
// volumes, and binds with nothing to hide alone.
func TestServiceShadowOverrideProjectsThePrimaryShadows(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "SECRET=canary\n")
	write(".ssh/id_ed25519", "key\n")
	write("config/prod.tfvars", "token = 1\n")
	write("data/public.csv", "1,2\n")
	write("data/notes.txt", "hidden by coopignore\n")
	write("data/.coopignore", "local.txt\n")
	write(".coopignore", "data/notes.txt\n")
	write("public/readme.md", "hello\n")
	if err := os.Symlink(filepath.Join(repo, ".env"), filepath.Join(repo, "alias")); err != nil {
		t.Fatal(err)
	}
	write(".agent/compose.yml", `services:
  db:
    image: postgres:18
    volumes:
      - "../:/repo:ro"
      - "../data:/data"
      - "../.env:/env:ro"
      - "../alias:/aliased:ro"
      - "../public:/public:ro"
      - "pgdata:/var/lib/postgresql"
  clean:
    image: alpine
    volumes: ["../public:/public:ro"]
volumes:
  pgdata:
`)
	compose := filepath.Join(repo, ".agent", "compose.yml")
	if err := ValidateComposeFile(compose, repo, false); err != nil {
		t.Fatalf("fixture compose must validate: %v", err)
	}
	data, err := os.ReadFile(compose)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	dir := t.TempDir()
	path, needed, err := serviceShadowOverride(repo, compose, data, dir)
	if err != nil || !needed {
		t.Fatalf("override = %q, needed=%v, err=%v", path, needed, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	override := string(body)
	decoyFile, decoyDir, err := serviceDecoyPaths()
	if err != nil {
		t.Fatal(err)
	}
	// The sidecars outlive this command, so their decoys must outlive the private per-start dir.
	if strings.HasPrefix(decoyFile, dir) || strings.HasPrefix(decoyDir, dir) {
		t.Fatalf("decoy sources %q/%q live in the per-start dir %q", decoyFile, decoyDir, dir)
	}
	for _, want := range []struct{ target, source string }{
		{"/repo/.env", decoyFile},               // hidden descendant of a directory bind
		{"/repo/.ssh", decoyDir},                // a secret directory is shadowed whole
		{"/repo/config/prod.tfvars", decoyFile}, // nested secret
		{"/repo/data/notes.txt", decoyFile},     // .coopignore applies through the bind
		{"/data/notes.txt", decoyFile},          // ... and under a narrower bind of that dir
		{"/env", decoyFile},                     // a direct bind of a secret is replaced
		{"/aliased", decoyFile},                 // through a symlink alias too
	} {
		if !strings.Contains(override, "target: "+quoted(want.target)) {
			t.Errorf("no decoy at %s:\n%s", want.target, override)
			continue
		}
		block := override[strings.Index(override, "target: "+quoted(want.target))-len("        source: "+quoted(want.source))-1:]
		if !strings.HasPrefix(strings.TrimSpace(block), "source: "+quoted(want.source)) {
			t.Errorf("decoy at %s does not use %s:\n%s", want.target, want.source, override)
		}
	}
	realRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ target, source string }{
		{"/repo/.coopignore", filepath.Join(realRepo, ".coopignore")},
		{"/repo/data/.coopignore", filepath.Join(realRepo, "data", ".coopignore")},
		{"/data/.coopignore", filepath.Join(realRepo, "data", ".coopignore")},
	} {
		block := "source: " + quoted(want.source) + "\n        target: " + quoted(want.target) + "\n        read_only: true"
		if !strings.Contains(override, block) {
			t.Errorf("no read-only policy mount at %s:\n%s", want.target, override)
		}
	}
	for _, public := range []string{"/repo/.ssh/id_ed25519", "/repo/data/public.csv", "/repo/public/readme.md", "/public", "pgdata", "clean:"} {
		if strings.Contains(override, public) {
			t.Errorf("override touches %q, which has nothing to hide:\n%s", public, override)
		}
	}
	if info, err := os.Stat(decoyFile); err != nil || info.Size() != 0 {
		t.Fatalf("decoy file = %+v, %v; want an empty file", info, err)
	}

	// A compose file with nothing to hide writes no override at all.
	write("plain/compose.yml", "services:\n  db:\n    image: postgres:18\n    volumes: [\"pgdata:/var/lib/postgresql\", \"../public:/public:ro\"]\nvolumes:\n  pgdata:\n")
	plain := filepath.Join(repo, "plain", "compose.yml")
	data, _ = os.ReadFile(plain)
	if path, needed, err := serviceShadowOverride(repo, plain, data, t.TempDir()); err != nil || needed || path != "" {
		t.Fatalf("plain override = %q, needed=%v, err=%v; want none", path, needed, err)
	}
}

func quoted(s string) string { return `"` + s + `"` }

func TestServiceSecretApprovalKeepsPolicyReadOnly(t *testing.T) {
	policy := serviceDecoy{target: "/repo/.coopignore", source: ".coopignore", bindSource: "/repo/.coopignore"}
	secret := serviceDecoy{target: "/repo/.env", source: ".env"}
	kept, hidden := keepDecoysOutside(map[string][]serviceDecoy{"app": {policy, secret}}, []string{".coopignore", ".env"})
	if len(hidden) != 0 || len(kept["app"]) != 1 || kept["app"][0] != policy {
		t.Fatalf("kept=%+v hidden=%v, want only the policy mount", kept, hidden)
	}
}
