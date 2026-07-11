package loopcfg

import (
	"os"
	"path/filepath"
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
	if c == nil || len(c.Work.Agent) != 0 || c.Review.Rounds != 0 || c.Preflight.Enabled || c.Between.Enabled {
		t.Errorf("absent file should decode to a zero Config, got %+v", c)
	}
}

func TestLoadFull(t *testing.T) {
	repo := write(t, `
work:
  agent: [frontier, claude:opus, codex:gpt-5.6-luna]
  command: [make, loop-iter]
review:
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
	if c.Review.Rounds != 7 || len(c.Review.Agent) != 2 {
		t.Errorf("review = %+v", c.Review)
	}
	if !c.Preflight.Enabled || c.Preflight.Prompt == "" {
		t.Errorf("preflight = %+v", c.Preflight)
	}
	if !c.Between.Enabled || len(c.Between.Agent) != 1 || c.Between.Prompt == "" {
		t.Errorf("between = %+v", c.Between)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"unknown top-level key":     "reviews:\n  rounds: 5\n",
		"unknown nested key":        "review:\n  round: 5\n",
		"malformed target rung":     "work:\n  agent: [\"claude:opus:extra\"]\n",
		"preset rung with model":    "work:\n  agent: [frontier:opus]\n", // unknown provider 'frontier'
		"bad preset name":           "work:\n  agent: [\"has space\"]\n",
		"between enabled no prompt": "between:\n  enabled: true\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Errorf("expected error for:\n%s", body)
			}
		})
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
