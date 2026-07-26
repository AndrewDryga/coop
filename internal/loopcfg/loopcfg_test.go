package loopcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "loop.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// An absent file is not an error — it's "all built-in defaults" (today's behavior).
func TestLoadAbsent(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("absent loop.yaml should be nil error, got %v", err)
	}
	if c == nil || len(c.Work.Agent) != 0 || c.Signoff.Rounds != 0 || c.Preflight.Enabled || c.Between.Enabled {
		t.Errorf("absent file should decode to a zero Config, got %+v", c)
	}
	if c.MCPDisabled() {
		t.Error("absent file must not disable MCP (mcp is tri-state; nil = today's behavior)")
	}
}

// mcp: is tri-state — only an explicit false disables; absent and explicit true keep today's
// behavior (MCP mounts whenever mcp.json has servers).
func TestLoadMCP(t *testing.T) {
	for body, wantDisabled := range map[string]bool{
		"mcp: false\n":            true,
		"mcp: true\n":             false,
		"work:\n  agent: [codex]": false, // key absent
	} {
		c, err := Load(write(t, body))
		if err != nil {
			t.Fatalf("Load(%q): %v", body, err)
		}
		if c.MCPDisabled() != wantDisabled {
			t.Errorf("MCPDisabled() with %q = %v, want %v", body, c.MCPDisabled(), wantDisabled)
		}
	}
}

func TestLoadFull(t *testing.T) {
	repo := write(t, `
work:
  agent: [frontier, claude:opus, codex:gpt-5.6-luna]
  command: [make, loop-iter]
signoff:
  rounds: 7
  agent: [codex:gpt-5.6-sol/xhigh, claude:fable]
  prompt: |
    - The changelog is updated.
preflight:
  enabled: true
  prompt: |
    Also drop stale screenshots.
between:
  enabled: true
  agent: [claude:sonnet]
  writes: repo
  prompt: |
    Audit the finished task.
`)
	c, err := Load(repo)
	if err != nil {
		t.Fatalf("valid config should load: %v", err)
	}
	if len(c.Work.Agent) != 3 || c.Work.Agent[0] != "frontier" {
		t.Errorf("work.agent = %v", c.Work.Agent)
	}
	if c.Signoff.Rounds != 7 || len(c.Signoff.Agent) != 2 {
		t.Errorf("signoff = %+v", c.Signoff)
	}
	if !c.Preflight.Enabled || c.Preflight.Prompt == "" {
		t.Errorf("preflight = %+v", c.Preflight)
	}
	if !c.Between.Enabled || len(c.Between.Agent) != 1 || c.Between.Prompt == "" {
		t.Errorf("between = %+v", c.Between)
	}
	if !c.Between.Writes.RepositoryWritable() {
		t.Errorf("between.writes = %q, want repo", c.Between.Writes)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"unknown top-level key":     "signoffs:\n  rounds: 5\n",
		"unknown nested key":        "signoff:\n  round: 5\n",
		"malformed target rung":     "work:\n  agent: [\"claude:opus:extra\"]\n",
		"preset rung with model":    "work:\n  agent: [frontier:opus]\n", // unknown provider 'frontier'
		"bad preset name":           "work:\n  agent: [\"has space\"]\n",
		"between enabled no prompt": "between:\n  enabled: true\n",
		"unknown review writes":     "signoff:\n  writes: source\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Errorf("expected error for:\n%s", body)
			}
		})
	}
}

// LoadSnapshot digests the same bytes the Config was parsed from; an absent file gets the
// explicit built-in-defaults state instead of a digest.
func TestLoadSnapshotState(t *testing.T) {
	body := "signoff:\n  rounds: 7\n"
	repo := write(t, body)
	c, snap, err := LoadSnapshot(repo)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if c.Signoff.Rounds != 7 {
		t.Errorf("snapshot config = %+v, want the parsed file", c)
	}
	if want := File + " (sha256 " + Digest([]byte(body)) + ")"; snap.State() != want {
		t.Errorf("State() = %q, want %q", snap.State(), want)
	}
	if _, absentSnap, err := LoadSnapshot(t.TempDir()); err != nil || absentSnap.State() != File+" absent — built-in defaults" {
		t.Errorf("absent State() = %q, %v", absentSnap.State(), err)
	}
}

// A mid-run edit drifts ONCE per new digest — the run keeps its startup snapshot, so the same
// changed bytes must not warn again at every later box launch, while an edit-of-the-edit must.
func TestSnapshotDrift(t *testing.T) {
	original := "signoff:\n  rounds: 7\n"
	repo := write(t, original)
	_, snap, err := LoadSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, ".agent", "loop.yaml")
	if warning, drifted := snap.Drift(); drifted {
		t.Errorf("unchanged file drifted: %q", warning)
	}
	// Drift compares bytes, never re-parses: even an invalid edit only warns.
	edited := "signoff:\n  rounds: [not yaml"
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	warning, drifted := snap.Drift()
	if !drifted || !strings.Contains(warning, Digest([]byte(original))) ||
		!strings.Contains(warning, Digest([]byte(edited))) || !strings.Contains(warning, "restart to apply") {
		t.Errorf("edit drift = %q, %v", warning, drifted)
	}
	if warning, drifted := snap.Drift(); drifted {
		t.Errorf("same digest warned twice: %q", warning)
	}
	if err := os.WriteFile(path, []byte(edited+"\n# again"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, drifted := snap.Drift(); !drifted {
		t.Error("a second distinct edit must warn again")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if warning, drifted := snap.Drift(); drifted {
		t.Errorf("an earlier drift digest warned again: %q", warning)
	}
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if warning, drifted := snap.Drift(); drifted {
		t.Errorf("reverting to the startup bytes drifted: %q", warning)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	warning, drifted = snap.Drift()
	if !drifted || !strings.Contains(warning, "deleted mid-run") || !strings.Contains(warning, "restart to apply") {
		t.Errorf("delete drift = %q, %v", warning, drifted)
	}
	if warning, drifted := snap.Drift(); drifted {
		t.Errorf("still-deleted file warned twice: %q", warning)
	}
}

// A run started WITHOUT loop.yaml warns when one appears — it will not be picked up mid-run.
func TestSnapshotDriftFromAbsent(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, snap, err := LoadSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	if warning, drifted := snap.Drift(); drifted {
		t.Errorf("still-absent file drifted: %q", warning)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "loop.yaml"), []byte("mcp: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	warning, drifted := snap.Drift()
	if !drifted || !strings.Contains(warning, "appeared mid-run") || !strings.Contains(warning, "built-in defaults") {
		t.Errorf("appear drift = %q, %v", warning, drifted)
	}
	if warning, drifted := snap.Drift(); drifted {
		t.Errorf("appeared file warned twice: %q", warning)
	}
}

func TestRungsClassify(t *testing.T) {
	rungs, err := Rungs([]string{"frontier", "claude", "claude:opus", "codex:gpt-5.6-sol/xhigh@work"})
	if err != nil {
		t.Fatalf("valid rungs: %v", err)
	}
	if rungs[0].Preset != "frontier" || rungs[0].Target != nil {
		t.Errorf("rung 0 should be preset frontier, got %+v", rungs[0])
	}
	if rungs[1].Target == nil || rungs[1].Target.Provider != "claude" || rungs[1].Preset != "" {
		t.Errorf("rung 1 should be a bare claude target, got %+v", rungs[1])
	}
	if rungs[2].Target == nil || rungs[2].Target.Model != "opus" {
		t.Errorf("rung 2 should be claude:opus, got %+v", rungs[2])
	}
	if rungs[3].Target == nil || rungs[3].Target.Effort != "xhigh" || len(rungs[3].Target.Accounts) != 1 {
		t.Errorf("rung 3 should carry effort + account, got %+v", rungs[3])
	}
	if r, err := Rungs(nil); err != nil || r != nil {
		t.Errorf("nil rungs → nil, nil; got %v, %v", r, err)
	}
}
