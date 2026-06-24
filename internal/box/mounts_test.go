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
	if m := find(mounts, "/workspace/secrets"); m == nil || m.Kind != DirDecoy {
		t.Errorf("secrets/ not shadowed by tmpfs: %+v", m)
	}
	if m := find(mounts, "/workspace/.ssh"); m == nil || m.Kind != DirDecoy {
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

// TestComputeMountsCoopIgnore covers the repo-local .coopignore extension: basename
// patterns (any depth), repo-relative path patterns (exact + glob, not matched
// elsewhere), a directory entry, comments/blanks, and that a template still wins.
func TestComputeMountsCoopIgnore(t *testing.T) {
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
	write(CoopIgnoreFile, "# project secrets\nprod.yml\n/config/creds.yaml\ndata/*.csv\nvault/\n\n")
	write("prod.yml", "s")                    // basename → shadow
	write("nested/prod.yml", "s")             // basename at depth → shadow
	write("config/creds.yaml", "s")           // exact path → shadow
	write("config/creds.yaml.example", "tpl") // template wins → visible
	write("other/creds.yaml", "ok")           // same name, different path → visible
	write("data/big.csv", "s")                // path glob → shadow
	write("vault/key", "s")                   // dir → tmpfs, pruned
	write("src/app.js", "ok")                 // ordinary source → visible

	mounts, err := ComputeMounts(root, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	shadowed := func(target string) bool { return find(mounts, target) != nil }

	for _, target := range []string{
		"/workspace/prod.yml", "/workspace/nested/prod.yml",
		"/workspace/config/creds.yaml", "/workspace/data/big.csv",
	} {
		if !shadowed(target) {
			t.Errorf("%s should be shadowed by .coopignore", target)
		}
	}
	if m := find(mounts, "/workspace/vault"); m == nil || m.Kind != DirDecoy {
		t.Errorf("vault/ should be a tmpfs: %+v", m)
	}
	if shadowed("/workspace/vault/key") {
		t.Error("vault/ contents must not be enumerated (dir is pruned)")
	}
	for _, target := range []string{
		"/workspace/other/creds.yaml",          // path pattern is config/creds.yaml only
		"/workspace/config/creds.yaml.example", // template stays visible
		"/workspace/src/app.js",
		"/workspace/" + CoopIgnoreFile, // the ignore file itself is not a secret
	} {
		if shadowed(target) {
			t.Errorf("%s must stay visible", target)
		}
	}
}

// A .coopignore in a subdirectory shadows only within its own subtree: its basename
// patterns apply at any depth under it, its path patterns are relative to it, and it
// has no effect on siblings or the repo root.
func TestComputeMountsSubdirCoopIgnore(t *testing.T) {
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
	write("sub/"+CoopIgnoreFile, "creds.yaml\nlocal/secret.txt\n") // basename + a sub-relative path
	write("sub/creds.yaml", "s")                                   // basename → shadow
	write("sub/deep/creds.yaml", "s")                              // basename, deeper → shadow
	write("sub/local/secret.txt", "s")                             // path relative to sub → shadow
	write("sub/local/other.txt", "ok")                             // not matched → visible
	write("creds.yaml", "ok")                                      // repo root: sub's rule doesn't reach here → visible
	write("other/creds.yaml", "ok")                                // a sibling subtree → visible

	mounts, err := ComputeMounts(root, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	shadowed := func(target string) bool { return find(mounts, target) != nil }

	for _, target := range []string{
		"/workspace/sub/creds.yaml", "/workspace/sub/deep/creds.yaml", "/workspace/sub/local/secret.txt",
	} {
		if !shadowed(target) {
			t.Errorf("%s should be shadowed by sub/.coopignore", target)
		}
	}
	for _, target := range []string{
		"/workspace/sub/local/other.txt", "/workspace/creds.yaml", "/workspace/other/creds.yaml",
	} {
		if shadowed(target) {
			t.Errorf("%s must stay visible (sub/.coopignore is scoped to sub/)", target)
		}
	}
}

func TestRenderMounts(t *testing.T) {
	mounts := []Mount{
		{Kind: Bind, Source: "/repo", Target: "/workspace"},
		{Kind: Decoy, Target: "/workspace/.env", RO: true},
		{Kind: DirDecoy, Target: "/workspace/secrets", RO: true},
	}
	got := RenderMounts(mounts, "/tmp/decoy", "/tmp/decoydir")
	want := []string{
		"-v", "/repo:/workspace",
		"-v", "/tmp/decoy:/workspace/.env:ro",
		"-v", "/tmp/decoydir:/workspace/secrets:ro",
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
		// expanded defaults
		{"release.keystore", true}, {"AuthKey.p8", true}, {"vault.kdbx", true}, {"id_dsa", true},
		{".pgpass", true}, {"home.ovpn", true}, {".dockercfg", true}, {".htpasswd", true}, {"server.ppk", true},
		{"app.js", false}, {"README.md", false}, {"env", false}, {"app.env", false}, {"cert.crt", false},
	}
	for _, c := range cases {
		if got := matchesAny(c.name, SecretGlobs); got != c.want {
			t.Errorf("matchesAny(%q, secrets) = %v, want %v", c.name, got, c.want)
		}
	}
}

// The broadened denylist catches high-confidence service-credential filenames the basename list
// missed (task: strengthen secret-shadowing), without adding public-cert / ordinary-config
// patterns that would break in-box TLS or hide config the agent needs.
func TestSecretGlobsServiceCredentials(t *testing.T) {
	shadowed := NewShadowDecider(t.TempDir()) // no .coopignore → just SecretGlobs/AllowGlobs
	for _, p := range []string{
		"credentials.json", "service_account.json", "gcp-sa.json", "config/app-sa.json",
		"kubeconfig", "config/database.yml",
		// YAML credential/secret files (.yaml and .yml).
		"config/credentials.yaml", "credentials.yml", "secrets.yaml", "config/secrets.yml",
		// Case variants must still shadow (case-insensitive built-in matching).
		".ENV", "ID_RSA", "config/Server.PEM", "CREDENTIALS.JSON",
	} {
		if !shadowed(p) {
			t.Errorf("%s should be shadowed by the broadened denylist", p)
		}
	}
	// Deliberately NOT added — public certs (break in-box TLS) and ordinary app config.
	for _, p := range []string{"server.crt", "ca.cer", "application.yml", "src/main.go", "package.json"} {
		if shadowed(p) {
			t.Errorf("%s must NOT be shadowed (public cert / ordinary config)", p)
		}
	}
	// A case variant of an AllowGlobs name (template/CA bundle) must still be allowed.
	for _, p := range []string{".ENV.EXAMPLE", "CACERTS.PEM"} {
		if shadowed(p) {
			t.Errorf("%s must NOT be shadowed (case variant of an allowed template/CA name)", p)
		}
	}
}

// TestCoopIgnoreOverridesAllowGlobs: an AllowGlobs name (public CA bundle, .env.example) stays
// visible by default, but an explicit .coopignore entry re-hides it — .coopignore is the user's
// final say. Ordinary secrets stay shadowed regardless.
func TestCoopIgnoreOverridesAllowGlobs(t *testing.T) {
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
	// The user explicitly hides two otherwise-allowed names.
	write(CoopIgnoreFile, "cacerts.pem\n.env.example\n")
	write("cacerts.pem", "bundle")
	write(".env.example", "KEY=placeholder")
	write("ca-bundle.crt", "bundle") // allowed AND not in .coopignore → stays visible
	write("src/app.js", "ok")

	mounts, err := ComputeMounts(root, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	shadowed := func(target string) bool { return find(mounts, target) != nil }

	for _, target := range []string{"/workspace/cacerts.pem", "/workspace/.env.example"} {
		if !shadowed(target) {
			t.Errorf("%s is in .coopignore and must be re-hidden despite AllowGlobs", target)
		}
	}
	for _, target := range []string{"/workspace/ca-bundle.crt", "/workspace/src/app.js"} {
		if shadowed(target) {
			t.Errorf("%s must stay visible (allowed, not in .coopignore)", target)
		}
	}
}

// Public CA bundles must stay visible — emptying a trusted bundle breaks in-box TLS verification —
// while genuine keys/secrets still shadow.
func TestPublicCABundlesNotShadowed(t *testing.T) {
	shadowed := NewShadowDecider(t.TempDir())
	for _, p := range []string{
		"cacerts.pem", "server/deps/castore/priv/cacerts.pem", "ca-bundle.crt", "ca-certificates.crt", "cacert.pem",
	} {
		if shadowed(p) {
			t.Errorf("%s is a public CA bundle and must NOT be shadowed (breaks in-box TLS)", p)
		}
	}
	for _, p := range []string{"deploy.pem", "tls.key", "config/id_rsa", ".env"} {
		if !shadowed(p) {
			t.Errorf("%s is a secret and must still be shadowed", p)
		}
	}
}
