package preset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePreset lays down .agent/presets/<name>/preset.yaml (plus extra files) in a
// temp repo and returns the repo root.
func writePreset(t *testing.T, name, yaml string, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	dir := filepath.Join(repo, ".agent", "presets", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

const frontierYAML = `
lead:
  agent: claude
  model: claude-fable-5
  credentials: [work]
  prompt: lead.md

roles:
  thinker:
    mode: native
    agent: claude
    model: claude-opus-4-8
    subagent: deep-reasoner
    when: [architecture, debugging]
    prompt: roles/thinker.md

  critic:
    mode: consult
    agent: codex
    model: gpt-5.5
    credentials: [work]
    when: [plan-review, security]

  fast:
    mode: delegate
    agent: gemini
    model: gemini-3.5-flash
    credentials: [work]
    when: [boilerplate, bulk-edits]
    commit: never
    concurrent: never
`

var frontierFiles = map[string]string{
	"lead.md":          "LEAD EXTRA",
	"roles/thinker.md": "THINKER EXTRA",
}

func TestLoadFrontier(t *testing.T) {
	repo := writePreset(t, "frontier", frontierYAML, frontierFiles)
	p, err := Load(repo, "frontier")
	if err != nil {
		t.Fatal(err)
	}
	if p.LeadAgent != "claude" || p.LeadModel != "claude-fable-5" {
		t.Errorf("lead = %s/%s", p.LeadAgent, p.LeadModel)
	}
	if len(p.LeadCredentials) != 1 || p.LeadCredentials[0] != "work" {
		t.Errorf("lead credentials = %v", p.LeadCredentials)
	}
	if p.LeadPromptText != "LEAD EXTRA" {
		t.Errorf("lead prompt = %q", p.LeadPromptText)
	}
	// Roles come back sorted by name: critic, fast, thinker.
	if len(p.Roles) != 3 || p.Roles[0].Name != "critic" || p.Roles[1].Name != "fast" || p.Roles[2].Name != "thinker" {
		t.Fatalf("roles = %+v", p.Roles)
	}
	if !p.HasConsult() || !p.HasDelegate() {
		t.Error("frontier has a consult and a delegate role")
	}
	if got := p.RoleAgents(); len(got) != 2 || got[0] != "codex" || got[1] != "gemini" {
		t.Errorf("RoleAgents = %v (native thinker must not add claude)", got)
	}
	th := p.Roles[2]
	if th.Subagent != "deep-reasoner" || th.PromptText != "THINKER EXTRA" {
		t.Errorf("thinker = %+v", th)
	}
	if d := p.Delegates(); len(d) != 1 || d[0].Name != "fast" || d[0].Agent != "gemini" {
		t.Errorf("Delegates = %+v", d)
	}
}

// Every rejected shape gets a clear, named error.
func TestLoadValidation(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		files   map[string]string
		wantErr string
	}{
		{"malformed yaml", "lead: [not\n  a: map", nil, "malformed YAML"},
		{"missing lead agent", "roles: {}", nil, "lead.agent is required"},
		{"unknown lead agent", "lead: {agent: gpt4}", nil, "not a known agent"},
		{"unknown role agent", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: wat}}", nil, "not a known agent"},
		{"missing mode", "lead: {agent: claude}\nroles: {r: {agent: codex}}", nil, "mode is required"},
		{"bad mode", "lead: {agent: claude}\nroles: {r: {mode: boss, agent: codex}}", nil, "not one of native, consult, delegate"},
		{"missing prompt file", "lead: {agent: claude, prompt: lead.md}", nil, "does not exist"},
		{"missing role prompt file", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: codex, prompt: roles/r.md}}", nil, "does not exist"},
		{"empty lead model", "lead: {agent: claude, model: \"\"}", nil, "model is empty"},
		{"empty role model", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: codex, model: \"\"}}", nil, "model is empty"},
		{"empty credentials list", "lead: {agent: claude, credentials: []}", nil, "is empty"},
		{"bad credential name", "lead: {agent: claude, credentials: [\"../x\"]}", nil, "invalid name"},
		{"singular credential", "lead: {agent: claude, credential: work}", nil, "plural list form"},
		{"singular role credential", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: codex, credential: work}}", nil, "plural list form"},
		{"native needs subagent", "lead: {agent: claude}\nroles: {r: {mode: native, agent: claude}}", nil, "needs subagent"},
		{"native is claude-only", "lead: {agent: claude}\nroles: {r: {mode: native, agent: codex, subagent: x}}", nil, "agent must be claude"},
		{"subagent on consult", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: codex, subagent: x}}", nil, "only applies to mode: native"},
		{"commit allow rejected", "lead: {agent: claude}\nroles: {r: {mode: delegate, agent: gemini, commit: allow}}", nil, "only 'never' is supported"},
		{"concurrent group rejected", "lead: {agent: claude}\nroles: {r: {mode: delegate, agent: gemini, concurrent: \"group:a\"}}", nil, "only 'never' is supported"},
		{"commit on consult", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: codex, commit: never}}", nil, "only apply to mode: delegate"},
		{"permissions rejected", "lead: {agent: claude}\nroles: {r: {mode: delegate, agent: gemini, permissions: rw}}", nil, "not supported"},
		{"write_paths rejected", "lead: {agent: claude}\nroles: {r: {mode: delegate, agent: gemini, write_paths: [a]}}", nil, "not supported"},
		{"bad role name", "lead: {agent: claude}\nroles: {Fast Role: {mode: consult, agent: codex}}", nil, "role name"},
		{"unknown field", "lead: {agent: claude, sidekick: yes}", nil, "malformed YAML"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := writePreset(t, "p", c.yaml, c.files)
			_, err := Load(repo, "p")
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

func TestLoadMissingPreset(t *testing.T) {
	if _, err := Load(t.TempDir(), "ghost"); err == nil || !strings.Contains(err.Error(), `no preset "ghost"`) {
		t.Errorf("missing preset: err = %v", err)
	}
	if _, err := Load(t.TempDir(), "../evil"); err == nil || !strings.Contains(err.Error(), "invalid preset name") {
		t.Errorf("traversal name: err = %v", err)
	}
}

// The generated lead contract carries the routing table and the EXACT invocations,
// with Markdown appended after (never replacing) the generated text.
func TestLeadContract(t *testing.T) {
	repo := writePreset(t, "frontier", frontierYAML, frontierFiles)
	p, err := Load(repo, "frontier")
	if err != nil {
		t.Fatal(err)
	}
	c := LeadContract(p)
	for _, want := range []string{
		`preset "frontier" — you are the lead (claude)`,
		"@deep-reasoner",             // native invocation
		"coop-consult codex --fresh", // consult invocation
		"coop-delegate fast <<'EOF'", // delegate invocation
		"NEVER commit",               // delegate safety text
		"review its `git diff`",      // lead owns review
		"Use for: architecture, debugging",
		"claude-opus-4-8", "gpt-5.5", "gemini-3.5-flash",
		"THINKER EXTRA", "LEAD EXTRA",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("lead contract missing %q:\n%s", want, c)
		}
	}
	// Markdown appends AFTER the generated role text, never replaces it.
	if strings.Index(c, "@deep-reasoner") > strings.Index(c, "THINKER EXTRA") {
		t.Error("role prompt must append after the generated role contract")
	}
	if strings.Index(c, "LEAD EXTRA") < strings.Index(c, "coop-delegate fast") {
		t.Error("lead.md must append after the generated block")
	}
}

func TestEnvKey(t *testing.T) {
	if got := EnvKey("fast-writer"); got != "FAST_WRITER" {
		t.Errorf("EnvKey = %q, want FAST_WRITER", got)
	}
}
