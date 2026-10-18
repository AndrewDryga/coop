package box

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A sidecar bind of the repo must not hand the sidecar the raw files the box only sees as
// decoys: the override shadows every hidden descendant of a directory bind, replaces a bind whose
// source is itself a secret (through a symlink alias too), and leaves public files, named
// volumes, and binds with nothing to hide alone.
func TestServiceShadowOverrideProjectsThePrimaryShadows(t *testing.T) {
	pinHome(t)
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
	rootPolicy, err := durableServicePolicySnapshot([]byte("data/notes.txt\n"))
	if err != nil {
		t.Fatal(err)
	}
	childPolicy, err := durableServicePolicySnapshot([]byte("local.txt\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ target, source string }{
		{"/repo/.coopignore", rootPolicy},
		{"/repo/data/.coopignore", childPolicy},
		{"/data/.coopignore", childPolicy},
	} {
		block := "source: " + quoted(want.source) + "\n        target: " + quoted(want.target) + "\n        read_only: true"
		if !strings.Contains(override, block) {
			t.Errorf("no read-only policy mount at %s:\n%s", want.target, override)
		}
	}
	if err := os.Remove(filepath.Join(repo, ".coopignore")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(rootPolicy); err != nil || string(got) != "data/notes.txt\n" {
		t.Fatalf("durable policy disappeared with its repository source: %q, %v", got, err)
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

func TestServiceShadowCoversExplicitBareBind(t *testing.T) {
	repo, compose := writeCompose(t, "services:\n  probe:\n    image: alpine\n    volumes: [{type: bind, source: secret.key, target: /key, read_only: true}]\n")
	if err := os.WriteFile(filepath.Join(repo, ".agent", "secret.key"), []byte("fixture-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readValidatedCompose(compose, repo, true)
	if err != nil {
		t.Fatal(err)
	}
	decoys, hidden, _, err := serviceShadowPlan(repo, compose, data)
	if err != nil || len(hidden) != 1 || hidden[0] != ".agent/secret.key" || len(decoys["probe"]) != 1 || decoys["probe"][0].target != "/key" {
		t.Fatalf("explicit bare secret bind was not shadowed: decoys=%v hidden=%v err=%v", decoys, hidden, err)
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

// serviceShadowOverride extends the primary box's secret shadowing to sibling services. A
// validated compose file may bind any path inside the repo into a sidecar — the repo root, a
// directory, or a single file — and Compose would hand the sidecar the raw host files, including
// every .env, key, and .coopignore'd path the box itself only ever sees as an empty decoy. The
// override written here applies the SAME shadow decision (NewShadowDecider: SecretGlobs, AllowGlobs,
// .coopignore) to each bind: a bind whose resolved source is itself a secret is replaced by a decoy
// at the same target (Compose merges volumes by target, so the override wins), and a directory bind
// gets a decoy over every shadowed descendant, pruning shadowed directories whole. Symlink aliases
// are resolved before deciding, so `./alias -> .env` is shadowed like `.env`. Named volumes and
// tmpfs entries carry no host source and are untouched.
//
// Returns the override path and true when at least one decoy was needed; decoy sources live in
// dir, the private per-start temp dir, beside the frozen compose snapshot.
func serviceShadowOverride(repo, composeFile string, data []byte, dir string) (string, bool, error) {
	decoys, _, _, err := serviceShadowPlan(repo, composeFile, data)
	if err != nil {
		return "", false, err
	}
	return writeServiceShadowOverride(repo, decoys, dir)
}

// On a disk that ignores letter case a service opens a hidden file under any spelling of its name,
// so inside a folder bind every spelling gets the decoy and every spelling of a .coopignore the
// read-only policy. A name too long to cover, or past the service's bound, comes back for coop up
// to refuse. On a disk that keeps case none of this applies, so the test asks the disk which it is.
func TestServiceShadowCoversEverySpellingWhereTheDiskIgnoresCase(t *testing.T) {
	pinHome(t)
	repo, compose := writeCompose(t, "services:\n  app:\n    image: alpine\n    volumes: [\"../:/app:ro\"]\n")
	for rel, body := range map[string]string{".env": "SECRET=inert\n", ".coopignore": "notes/\n", "config/credentials.json": `{"token":"inert"}`} {
		writeRepoFile(t, filepath.Join(repo, rel), body)
	}
	folds := foldsCase(filepath.Join(repo, ".env"))
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	plan := func() (map[string][]serviceDecoy, []string, []string) {
		t.Helper()
		data, err := readValidatedCompose(compose, repo, true)
		if err != nil {
			t.Fatal(err)
		}
		decoys, hidden, uncovered, err := serviceShadowPlan(repo, compose, data)
		if err != nil {
			t.Fatal(err)
		}
		return decoys, hidden, uncovered
	}
	count := func(decoys []serviceDecoy, source string) int {
		n := 0
		for _, d := range decoys {
			if d.source == source {
				n++
			}
		}
		return n
	}

	decoys, hidden, uncovered := plan()
	wantEnv, wantPolicy := 1, 1
	var wantUncovered []string
	if folds {
		wantEnv, wantPolicy, wantUncovered = 1<<3, 1<<10, []string{"config/credentials.json"}
	}
	if got := count(decoys["app"], ".env"); got != wantEnv {
		t.Errorf(".env has %d mounts, want %d on this disk (folds case: %v)", got, wantEnv, folds)
	}
	if got := count(decoys["app"], ".coopignore"); got != wantPolicy {
		t.Errorf(".coopignore has %d mounts, want %d on this disk (folds case: %v)", got, wantPolicy, folds)
	}
	if count(decoys["app"], "config/credentials.json") != 1 || !slices.Equal(uncovered, wantUncovered) {
		t.Errorf("long name: %d mounts, uncovered %v; want its decoy kept and %v named", count(decoys["app"], "config/credentials.json"), uncovered, wantUncovered)
	}
	if !slices.Equal(hidden, []string{".env", "config/credentials.json"}) {
		t.Errorf("hidden = %v: covers of other spellings are no extra hidden paths", hidden)
	}
	path, _, err := writeServiceShadowOverride(repo, decoys, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoyFile, _, err := serviceDecoyPaths()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := durableServicePolicySnapshot([]byte("notes/\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		target, source string
		present        bool
	}{
		{"/app/.env", decoyFile, true}, {"/app/.coopignore", policy, true},
		{"/app/.ENV", decoyFile, folds}, {"/app/.COOPIGNORE", policy, folds},
	} {
		mount := "source: " + quoted(want.source) + "\n        target: " + quoted(want.target) + "\n        read_only: true"
		if strings.Contains(string(body), mount) != want.present {
			t.Errorf("override mount at %s from %s: want present %v (folds case: %v)", want.target, want.source, want.present, folds)
		}
	}
	if stored, err := os.ReadDir(filepath.Dir(policy)); err != nil || len(stored) != 1 {
		t.Errorf("stored policies = %v, %v; want one copy however many spellings bind it", stored, err)
	}
	if !folds {
		return
	}

	// Covers stop at the service's bound: past it a name is named, never half covered.
	for i := range 9 {
		writeRepoFile(t, filepath.Join(repo, fmt.Sprintf("k%dabcdef.pem", i)), "key\n")
	}
	decoys, hidden, uncovered = plan()
	// One decoy per hidden file, one policy mount, and the rest are covers.
	if covers := len(decoys["app"]) - len(hidden) - 1; covers > serviceCoverLimit {
		t.Fatalf("app has %d covers, more than its bound of %d", covers, serviceCoverLimit)
	}
	for i := range 9 {
		name := fmt.Sprintf("k%dabcdef.pem", i)
		if got := count(decoys["app"], name); got != 1<<10 && !slices.Contains(uncovered, name) {
			t.Errorf("%s has %d mounts and is not named uncovered: half covered", name, got)
		}
	}
	if !slices.Contains(uncovered, "k8abcdef.pem") {
		t.Errorf("uncovered = %v; want the names past the bound named", uncovered)
	}
}
