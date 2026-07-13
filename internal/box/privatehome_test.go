package box

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// seedTestProfile fakes a codex profile: small durable files, the sqlite family this whole
// mechanism exists to isolate, and dirs of both kinds (shared vs ephemera).
func seedTestProfile(t *testing.T, dir string) {
	t.Helper()
	for name, body := range map[string]string{
		"auth.json":          `{"tokens":{}}`,
		"config.toml":        "model = \"gpt-5.5\"\n",
		"version.json":       `{"version":"0.144.1"}`,
		"installation_id":    "abc",
		"state_5.sqlite":     "sqlite",
		"state_5.sqlite-wal": "wal",
		"logs_2.sqlite":      "sqlite",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"sessions", "cache", ".tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSeedPrivateHome: the scratch home gets the profile's small top-level files (so the CLI
// skips first-run setup) but NEVER the sqlite family (the per-box state) nor the shared paths
// (they're bind-mounted through), and never dirs (durable ones are shared, the rest ephemera).
func TestSeedPrivateHome(t *testing.T) {
	profile, scratch := t.TempDir(), t.TempDir()
	seedTestProfile(t, profile)
	if err := seedPrivateHome(profile, scratch, []string{"auth.json", "sessions/"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"config.toml", "version.json", "installation_id"} {
		if _, err := os.Stat(filepath.Join(scratch, want)); err != nil {
			t.Errorf("scratch home missing %s: %v", want, err)
		}
	}
	for _, absent := range []string{"auth.json", "state_5.sqlite", "state_5.sqlite-wal", "logs_2.sqlite", "sessions", "cache"} {
		if _, err := os.Stat(filepath.Join(scratch, absent)); err == nil {
			t.Errorf("scratch home must not carry %s", absent)
		}
	}
}

// TestPrivateHomeMountArgs: the scratch mounts at the agent's home; shared files bind through
// only when they exist; shared dirs are created host-side so the bind can't fail; and a dir
// another mount already covers (the ACP sessions overlay) is skipped — never duplicated.
func TestPrivateHomeMountArgs(t *testing.T) {
	profile := t.TempDir()
	seedTestProfile(t, profile)
	shared := []string{"auth.json", "sessions/", "rules/"}

	args := privateHomeMountArgs(profile, "/scratch", "/home/node", "codex", shared, nil)
	want := []string{
		"-v", "/scratch:/home/node/.codex",
		"-v", filepath.Join(profile, "auth.json") + ":/home/node/.codex/auth.json",
		"-v", filepath.Join(profile, "sessions") + ":/home/node/.codex/sessions",
		"-v", filepath.Join(profile, "rules") + ":/home/node/.codex/rules",
	}
	if !slices.Equal(args, want) {
		t.Errorf("mount args:\n got %v\nwant %v", args, want)
	}
	// rules/ didn't exist — the bind must have created it host-side.
	if fi, err := os.Stat(filepath.Join(profile, "rules")); err != nil || !fi.IsDir() {
		t.Errorf("missing shared dir should be created on the profile: %v", err)
	}
	// A missing shared FILE is skipped (an API-key login has no auth.json).
	os.Remove(filepath.Join(profile, "auth.json"))
	if args := privateHomeMountArgs(profile, "/s", "/h", "codex", shared, nil); slices.ContainsFunc(args, func(a string) bool {
		return strings.Contains(a, "auth.json")
	}) {
		t.Errorf("missing auth.json must not be bound: %v", args)
	}
	// The ACP lead's sessions overlay covers sessions/ — skip it here, no duplicate target.
	args = privateHomeMountArgs(profile, "/s", "/h", "codex", shared, map[string]bool{"sessions": true})
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasSuffix(a, "/.codex/sessions") }) {
		t.Errorf("skipped dir still bound: %v", args)
	}
}

// TestPrivateHomesScope: only agents that DECLARE single-writer home state (codex) get a
// private home; claude shares as before; a Homes-less run gets none at all.
func TestPrivateHomesScope(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir, HomeInBox: "/home/node"}
	for _, p := range []string{"codex/profiles/default", "claude/profiles/default"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	seedTestProfile(t, filepath.Join(dir, "codex", "profiles", "default"))

	homes, tmp := privateHomes(cfg, RunSpec{Homes: true, Agent: "codex"})
	if homes["codex"] == "" || len(tmp) != 1 {
		t.Fatalf("codex must get a private home, got %v (tmp %v)", homes, tmp)
	}
	defer os.RemoveAll(homes["codex"])
	if _, err := os.Stat(filepath.Join(homes["codex"], "config.toml")); err != nil {
		t.Errorf("private home not seeded: %v", err)
	}
	if homes, _ := privateHomes(cfg, RunSpec{Homes: true, Agent: "claude"}); len(homes) != 0 {
		t.Errorf("claude shares its home — no private home expected, got %v", homes)
	}
	if homes, _ := privateHomes(cfg, RunSpec{Homes: false, Agent: "codex"}); homes != nil {
		t.Errorf("a Homes-less run mounts no agent homes at all, got %v", homes)
	}
}

// TestAssembleArgsPrivateHome: end-to-end through assembleArgs — the codex home mount is the
// scratch dir plus the shared binds, and with an ACP supervisor the sessions target appears
// exactly once (the shared ACP overlay, not the profile bind).
func TestAssembleArgsPrivateHome(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir, HomeInBox: "/home/node", Egress: "open"}
	profile := filepath.Join(dir, "codex", "profiles", "default")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	seedTestProfile(t, profile)
	priv := map[string]string{"codex": t.TempDir()}
	mounts := []Mount{{Kind: Bind, Source: "/r", Target: "/workspace"}}

	spec := RunSpec{Image: "i", Repo: "/r", Agent: "codex", Homes: true}
	got := strings.Join(assembleArgs(cfg, spec, mounts, "/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, "", "", priv), " ")
	if !strings.Contains(got, priv["codex"]+":/home/node/.codex") {
		t.Errorf("scratch home not mounted:\n%s", got)
	}
	if strings.Contains(got, profile+":/home/node/.codex ") {
		t.Errorf("profile must not mount as the whole home:\n%s", got)
	}
	if !strings.Contains(got, filepath.Join(profile, "auth.json")+":/home/node/.codex/auth.json") {
		t.Errorf("auth.json not bound from the profile:\n%s", got)
	}
	// ACP: the shared overlay owns sessions; the profile bind steps aside — exactly one target.
	spec.SupervisorID = "sup1"
	spec.ForceNoTTY = true
	got = strings.Join(assembleArgs(cfg, spec, mounts, "/d", "/dd", "/workspace", ttyStdinOnly, false, nil, nil, nil, nil, "", "", priv), " ")
	if n := strings.Count(got, ":/home/node/.codex/sessions"); n != 1 {
		t.Errorf("sessions target must appear exactly once, got %d:\n%s", n, got)
	}
	if strings.Contains(got, filepath.Join(profile, "sessions")+":") {
		t.Errorf("ACP box must use the shared sessions overlay, not the profile's:\n%s", got)
	}
}
