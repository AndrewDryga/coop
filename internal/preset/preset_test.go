package preset

import (
	"os"
	"path/filepath"
	"slices"
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
  agent: [claude:claude-fable-5, claude:claude-opus-4-8@work]
  prompt: lead.md

roles:
  thinker:
    mode: native
    agent: claude:claude-opus-4-8
    subagent: deep-reasoner
    when: [architecture, debugging]
    prompt: roles/thinker.md

  critic:
    mode: consult
    agent: codex:gpt-5.5
    when: [plan-review, security]

  fast:
    mode: delegate
    agent: gemini:gemini-3.5-flash
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
	p, err := Load(repo, "", "frontier")
	if err != nil {
		t.Fatal(err)
	}
	if p.LeadAgent != "claude" || p.LeadModel() != "claude-fable-5" {
		t.Errorf("lead = %s/%s", p.LeadAgent, p.LeadModel())
	}
	if len(p.LeadLadder) != 2 || p.LeadLadder[0].String() != "claude:claude-fable-5" || p.LeadLadder[1].String() != "claude:claude-opus-4-8@work" {
		t.Errorf("lead ladder = %v", p.LeadLadder)
	}
	if p.LeadPromptText != "LEAD EXTRA" {
		t.Errorf("lead prompt = %q", p.LeadPromptText)
	}
	// Roles come back sorted by name: critic, fast, thinker.
	if len(p.Roles) != 3 || p.Roles[0].Name != "critic" || p.Roles[1].Name != "fast" || p.Roles[2].Name != "thinker" {
		t.Fatalf("roles = %+v", p.Roles)
	}
	if len(p.ConsultRoles("claude")) == 0 || len(p.Delegates()) == 0 {
		t.Error("frontier has a consult and a delegate role")
	}
	if got := p.RunnableRoleAgents("claude"); len(got) != 2 || got[0] != "codex" || got[1] != "gemini" {
		t.Errorf("RunnableRoleAgents = %v (native thinker must not add claude)", got)
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
		{"missing lead agent", "roles: {}", nil, "lead.agent: is required"},
		{"unknown lead agent", "lead: {agent: gpt4}", nil, "unknown provider"},
		{"unknown role agent", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: wat}}", nil, "unknown provider"},
		{"missing mode", "lead: {agent: claude}\nroles: {r: {agent: codex}}", nil, "mode is required"},
		{"bad mode", "lead: {agent: claude}\nroles: {r: {mode: boss, agent: codex}}", nil, "not one of native, consult, delegate"},
		{"missing prompt file", "lead: {agent: claude, prompt: lead.md}", nil, "does not exist"},
		{"missing role prompt file", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: codex, prompt: roles/r.md}}", nil, "does not exist"},
		{"lead model unknown", "lead: {agent: claude, model: opus}", nil, "malformed YAML"},
		{"lead models unknown", "lead: {agent: claude, models: [x]}", nil, "malformed YAML"},
		{"lead credentials unknown", "lead: {agent: claude, credentials: [work]}", nil, "malformed YAML"},
		{"empty lead agent list", "lead: {agent: []}", nil, "empty list"},
		{"empty model in lead target", "lead: {agent: \"claude:\"}", nil, "empty model"},
		{"bad account in lead target", "lead: {agent: \"claude:opus@../x\"}", nil, "invalid account"},
		{"empty account after at", "lead: {agent: \"claude:opus@\"}", nil, "empty account"},
		{"role model unknown", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: codex, model: opus}}", nil, "malformed YAML"},
		{"empty role ladder", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: []}}", nil, "empty list"},
		{"native role ladder rejected", "lead: {agent: claude}\nroles: {r: {mode: native, agent: [claude:sonnet, claude:opus]}}", nil, "native subagents have no fallback hook"},
		{"role ladder map entry rejected", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: [codex, {p: gemini}]}}", nil, "agent[1] must be a target"},
		{"role agent map rejected", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: {p: codex}}}", nil, "not a map"},
		{"role account rejected", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: codex@work}}", nil, "default account"},
		{"role credentials unknown", "lead: {agent: claude}\nroles: {r: {mode: consult, agent: codex, credentials: [work]}}", nil, "malformed YAML"},
		{"native needs a capable agent", "lead: {agent: claude}\nroles: {r: {mode: native, agent: codex}}", nil, "supports in-session subagents"},
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
			_, err := Load(repo, "", "p")
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

func TestLoadRoleFallbackLadder(t *testing.T) {
	repo := writePreset(t, "ladder", `
lead: {agent: claude}
roles:
  critic:
    mode: consult
    agent: [codex:gpt-5.6-sol/xhigh, grok:grok-4.5/high, gemini]
`, nil)
	p, err := Load(repo, "", "ladder")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Roles) != 1 {
		t.Fatalf("roles = %+v, want one", p.Roles)
	}
	r := p.Roles[0]
	want := []string{"codex:gpt-5.6-sol/xhigh", "grok:grok-4.5/high", "gemini"}
	if len(r.TargetLadder()) != len(want) {
		t.Fatalf("role ladder = %v, want %v", r.TargetLadder(), want)
	}
	for i, target := range r.TargetLadder() {
		if got := target.String(); got != want[i] {
			t.Errorf("role ladder[%d] = %q, want %q", i, got, want[i])
		}
	}
	if r.Agent != "codex" || r.Model != "gpt-5.6-sol" || r.Effort != "xhigh" {
		t.Errorf("primary projection = %s:%s/%s, want first rung", r.Agent, r.Model, r.Effort)
	}
	if got := r.TargetList(); got != strings.Join(want, " ") {
		t.Errorf("TargetList = %q, want %q", got, strings.Join(want, " "))
	}
	if got := p.RunnableRoleAgents("claude"); !slices.Equal(got, []string{"codex", "grok", "gemini"}) {
		t.Errorf("RunnableRoleAgents = %v, want every fallback provider", got)
	}
}

// A lead agent: ladder MAY be cross-provider — the loop rotates across agents. The lead is the
// first rung's provider; every rung is the whole parsed target (one type, one grammar), so a
// cross-provider rung simply carries its own Provider and rotation swaps the agent on it.
func TestLoadCrossProviderLead(t *testing.T) {
	repo := writePreset(t, "x", "lead: {agent: [claude:opus, codex:gpt-5.5@work]}\n", nil)
	p, err := Load(repo, "", "x")
	if err != nil {
		t.Fatal(err)
	}
	if p.LeadAgent != "claude" {
		t.Errorf("LeadAgent = %q, want claude (the first rung)", p.LeadAgent)
	}
	if len(p.LeadLadder) != 2 {
		t.Fatalf("LeadLadder = %+v, want 2 rungs", p.LeadLadder)
	}
	if got := p.LeadLadder[0].String(); got != "claude:opus" {
		t.Errorf("rung 0 = %q, want claude:opus", got)
	}
	if got := p.LeadLadder[1].String(); got != "codex:gpt-5.5@work" {
		t.Errorf("rung 1 = %q, want codex:gpt-5.5@work", got)
	}
}

func TestLoadMissingPreset(t *testing.T) {
	if _, err := Load(t.TempDir(), "", "ghost"); err == nil || !strings.Contains(err.Error(), `no preset "ghost"`) {
		t.Errorf("missing preset: err = %v", err)
	}
	if _, err := Load(t.TempDir(), "", "../evil"); err == nil || !strings.Contains(err.Error(), "invalid preset name") {
		t.Errorf("traversal name: err = %v", err)
	}
}

// The generated lead contract carries the routing table and the EXACT invocations,
// with Markdown appended after (never replacing) the generated text.
func TestLeadContract(t *testing.T) {
	repo := writePreset(t, "frontier", frontierYAML, frontierFiles)
	p, err := Load(repo, "", "frontier")
	if err != nil {
		t.Fatal(err)
	}
	c := LeadContract(p, "claude")
	for _, want := range []string{
		`preset "frontier" — you are the lead (claude)`,
		"@deep-reasoner",              // native invocation
		"coop-consult critic --fresh", // consult invocation — role-addressed, like every role
		"coop-delegate fast <<'EOF'",  // delegate invocation
		"NEVER commit",                // delegate safety text
		"review its `git diff`",       // lead owns review
		"Use for: architecture, debugging",
		"one-line status is not the reply",
		"poll that same session to terminal exit",
		"complete accumulated output",
		"claude-opus-4-8", "gpt-5.5", "gemini-3.5-flash",
		"THINKER EXTRA", "LEAD EXTRA",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("lead contract missing %q:\n%s", want, c)
		}
	}
	// A consult role is addressed by ROLE name (the wrapper resolves its agent/model/persona
	// from COOP_CONSULT_<ROLE>_*), never by its agent.
	if strings.Contains(c, "coop-consult codex") {
		t.Errorf("consult roles are role-addressed, not agent-addressed:\n%s", c)
	}
	// A non-Claude lead can't host native subagents in-session, so the thinker DEGRADES to a
	// role-addressed read-only consult (coop-consult thinker) instead of @-delegation; the
	// consult/delegate roles stay as they are.
	cx := LeadContract(p, "codex")
	if strings.Contains(cx, "@deep-reasoner") {
		t.Errorf("native role must not @-delegate under a codex lead:\n%s", cx)
	}
	if !strings.Contains(cx, "coop-consult thinker --fresh") {
		t.Errorf("native thinker should degrade to `coop-consult thinker` under a codex lead:\n%s", cx)
	}
	if n := strings.Count(cx, "poll that same session to terminal exit"); n < 2 {
		t.Errorf("consult and degraded-native roles both need yielded-session guidance, got %d:\n%s", n, cx)
	}
	if !strings.Contains(cx, "coop-consult critic") || !strings.Contains(cx, "coop-delegate fast") {
		t.Errorf("consult/delegate roles should survive a codex lead:\n%s", cx)
	}
	// A degraded native's prompt becomes the consult persona (ConsultBody), so it must not
	// also dump into the lead contract.
	if strings.Contains(cx, "THINKER EXTRA") {
		t.Errorf("a degraded native's prompt belongs in its persona, not the lead contract:\n%s", cx)
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

// Scaffold writes the documented frontier template, which must LOAD as written — real
// model ids, all three role modes, and ACTIVE prompt: lines resolved by the starter
// prompt files Scaffold writes alongside preset.yaml — so a scaffolded preset lists and
// runs immediately. It never clobbers, and rejects names that could never round-trip
// (the presets command's own verbs included).
func TestScaffold(t *testing.T) {
	repo := t.TempDir()
	path, err := Scaffold(repo, "frontier")
	if err != nil {
		t.Fatal(err)
	}
	if path != Path(repo, "", "frontier") {
		t.Errorf("path = %q, want %q", path, Path(repo, "", "frontier"))
	}
	p, err := Load(repo, "", "frontier")
	if err != nil {
		t.Fatalf("the scaffolded template must load cleanly: %v", err)
	}
	if p.LeadAgent != "claude" || p.LeadModel() != "claude-fable-5" {
		t.Errorf("template lead = %s/%s", p.LeadAgent, p.LeadModel())
	}
	wantLadder := []string{"claude:claude-fable-5/xhigh", "codex:gpt-5.6-sol/xhigh"}
	if len(p.LeadLadder) != len(wantLadder) {
		t.Fatalf("template lead ladder = %v, want %v", p.LeadLadder, wantLadder)
	}
	for i, want := range wantLadder {
		if got := p.LeadLadder[i].String(); got != want {
			t.Errorf("template lead ladder[%d] = %q, want %q", i, got, want)
		}
	}
	if len(p.Roles) != 3 || len(p.ConsultRoles("claude")) == 0 || len(p.Delegates()) == 0 {
		t.Errorf("template should carry all three role modes: %+v", p.Roles)
	}
	wantRoles := map[string]struct{ agent, model, effort string }{
		"thinker": {"claude", "claude-opus-4-8", "xhigh"},
		"critic":  {"codex", "gpt-5.6-sol", "xhigh"},
		"fast":    {"gemini", "gemini-3.5-flash", ""},
	}
	for _, role := range p.Roles {
		want, ok := wantRoles[role.Name]
		if !ok {
			t.Errorf("unexpected scaffolded role %q", role.Name)
			continue
		}
		if role.Agent != want.agent || role.Model != want.model || role.Effort != want.effort {
			t.Errorf("template role %s target = %s:%s/%s, want %s:%s/%s",
				role.Name, role.Agent, role.Model, role.Effort, want.agent, want.model, want.effort)
		}
	}
	// The active prompt: lines must resolve — every file the template references is
	// written by Scaffold, and its text is appended to the contract.
	if p.LeadPromptText == "" {
		t.Error("scaffolded lead.md should load (LeadPromptText is empty)")
	}
	for _, r := range p.Roles {
		if r.Mode == ModeDelegate && r.PromptText == "" {
			t.Errorf("scaffolded roles/%s.md should load (PromptText is empty)", r.Name)
		}
	}
	for _, rel := range []string{"roles/lead.md", "roles/fast.md"} {
		if _, err := os.Stat(filepath.Join(repo, Dir, "frontier", filepath.FromSlash(rel))); err != nil {
			t.Errorf("Scaffold should write %s: %v", rel, err)
		}
	}
	// The header names the chosen preset so the run hints are copy-pasteable.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "coop loop frontier") {
		t.Errorf("template header should name the preset:\n%s", data)
	}
	// Never clobbers; validates the name.
	if _, err := Scaffold(repo, "frontier"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("re-scaffold should refuse: %v", err)
	}
	for _, bad := range []string{"", "init", "ls", "../evil", "-x"} {
		if _, err := Scaffold(t.TempDir(), bad); err == nil {
			t.Errorf("Scaffold(%q) should refuse the name", bad)
		}
	}
}

// A native role with no subagent generates a coop-<role> subagent from itself (model from
// the role, description from when, body from the prompt); one WITH subagent references it,
// and the generated role's prompt lives in the subagent, not the lead contract.
func TestNativeSubagentGeneration(t *testing.T) {
	gen := Role{Name: "thinker", Mode: ModeNative, Agent: "claude", Model: "opus",
		When: []string{"architecture", "debugging"}, PromptText: "Think hard."}
	ref := Role{Name: "critic", Mode: ModeNative, Agent: "claude", Subagent: "deep-reasoner"}

	if got := SubagentName(&gen); got != "coop-thinker" {
		t.Errorf("generated name = %q, want coop-thinker", got)
	}
	if got := SubagentName(&ref); got != "deep-reasoner" {
		t.Errorf("referenced name = %q, want deep-reasoner", got)
	}

	p := &Preset{Roles: []Role{gen, ref, {Name: "fast", Mode: ModeDelegate, Agent: "gemini"}}}
	if nr := p.GeneratedNativeRoles("claude"); len(nr) != 1 || nr[0].Name != "thinker" {
		t.Fatalf("GeneratedNativeRoles = %+v, want only the generated native role", nr)
	}

	if got := NativeDescription(&gen); got != "Use for: architecture, debugging." {
		t.Errorf("native description = %q", got)
	}
	if body := NativeBody(&Role{Name: "x", Mode: ModeNative, Agent: "claude"}); !strings.Contains(body, "You are the x subagent") {
		t.Errorf("empty prompt should get a default body:\n%s", body)
	}

	// The lead contract invokes @coop-thinker (generated) and @deep-reasoner (referenced),
	// and doesn't dump the generated role's prompt into the contract.
	c := LeadContract(&Preset{Name: "t", LeadAgent: "claude", Roles: []Role{gen, ref}}, "claude")
	if !strings.Contains(c, "@coop-thinker") || !strings.Contains(c, "@deep-reasoner") {
		t.Errorf("contract invocations wrong:\n%s", c)
	}
	if strings.Contains(c, "Think hard.") {
		t.Errorf("generated role's prompt belongs in its subagent, not the lead contract:\n%s", c)
	}
}

// ConsultRoles is the single lead-aware view of role-addressed read-only runners. ConsultBody is
// each one's persona: a degraded native's NativeBody, or an explicit consult's own prompt.
func TestConsultWiredRoles(t *testing.T) {
	p := &Preset{Roles: []Role{
		{Name: "thinker", Mode: ModeNative, Agent: "claude", Model: "opus", PromptText: "Think hard."},
		{Name: "critic", Mode: ModeConsult, Agent: "codex", PromptText: "Be ruthless."},
		{Name: "scout", Mode: ModeConsult, Agent: "codex"}, // two consult roles on ONE agent — distinct wirings
	}}
	if b := ConsultBody(&Role{Name: "x", Mode: ModeNative}); !strings.Contains(b, "You are the x subagent") {
		t.Errorf("a promptless native should yield the default body, got %q", b)
	}
	// Explicit consult roles wire under a native-capable lead, each with its own persona (or none).
	cs := p.ConsultRoles("claude")
	if len(cs) != 2 || cs[0].Name != "critic" || cs[1].Name != "scout" {
		t.Fatalf("Consults = %+v, want [critic scout]", cs)
	}
	if ConsultBody(&cs[0]) != "Be ruthless." {
		t.Errorf("an explicit consult's persona is its prompt, got %q", ConsultBody(&cs[0]))
	}
	if ConsultBody(&cs[1]) != "" {
		t.Errorf("a promptless consult has no persona (the peer answers as itself), got %q", ConsultBody(&cs[1]))
	}
	// ConsultRoles is the effective, lead-aware list every caller uses for wrapper wiring:
	// explicit consults under Claude; the same list plus degraded natives under another lead.
	if got := p.ConsultRoles("claude"); len(got) != 2 || got[0].Name != "critic" || got[1].Name != "scout" {
		t.Errorf("ConsultRoles(claude) = %+v, want [critic scout]", got)
	}
	if got := p.ConsultRoles("codex"); len(got) != 3 || got[0].Name != "thinker" || got[1].Name != "critic" || got[2].Name != "scout" {
		t.Errorf("ConsultRoles(codex) = %+v, want preset-order [thinker critic scout]", got)
	} else if ConsultBody(&got[0]) != "Think hard." {
		t.Errorf("a degraded native's persona is its prompt, got %q", ConsultBody(&got[0]))
	}
	if got := p.RunnableRoleAgents("claude"); !slices.Equal(got, []string{"codex"}) {
		t.Errorf("RunnableRoleAgents(claude) = %v, want [codex]", got)
	}
	if got := p.RunnableRoleAgents("codex"); !slices.Equal(got, []string{"claude", "codex"}) {
		t.Errorf("RunnableRoleAgents(codex) = %v, want [claude codex]", got)
	}
}

func TestNativeRoleRequiresMatchingCapableLead(t *testing.T) {
	p := &Preset{Roles: []Role{{
		Name: "foreign", Mode: ModeNative, Agent: "codex", Model: "gpt-5.6",
		PromptText: "Review the boundary.",
	}}}
	if got := p.GeneratedNativeRoles("claude"); got != nil {
		t.Fatalf("Claude lead generated a Codex-native role: %+v", got)
	}
	consults := p.ConsultRoles("claude")
	if len(consults) != 1 || consults[0].Name != "foreign" {
		t.Fatalf("mismatched native role did not degrade to consult: %+v", consults)
	}
	if got := p.RunnableRoleAgents("claude"); !slices.Equal(got, []string{"codex"}) {
		t.Fatalf("degraded role credential scope = %v, want [codex]", got)
	}
	contract := LeadContract(p, "claude")
	if !strings.Contains(contract, "coop-consult foreign --fresh") || strings.Contains(contract, "@coop-foreign") {
		t.Fatalf("mismatched native role contract did not degrade:\n%s", contract)
	}
}

// writePresetIn lays down <root>/<name>/preset.yaml (plus extra files) under an
// arbitrary root — used to populate a global presets dir that is NOT <repo>/.agent/presets.
func writePresetIn(t *testing.T, root, name, yaml string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
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
}

// A minimal valid preset body, parameterized by the lead prompt so a global and a repo
// copy of the same name are distinguishable after Load.
func soloYAML(prompt string) string {
	y := "lead:\n  agent: claude\n"
	if prompt != "" {
		y += "  prompt: " + prompt + "\n"
	}
	return y
}

// Global presets: a preset loads from a second, global root; a same-named repo preset
// SHADOWS it (repo wins); List unions + dedups + sorts; Path/Origin resolve the winning
// root; a global preset's prompt files resolve under the global folder; a name in neither
// root errors and names both searched locations.
func TestGlobalPresets(t *testing.T) {
	repo := t.TempDir()
	global := t.TempDir()

	// global-only preset, with a prompt file that must resolve UNDER the global folder.
	writePresetIn(t, global, "orch", soloYAML("lead.md"), map[string]string{"lead.md": "GLOBAL LEAD"})
	// a name present in BOTH roots — repo must win.
	writePresetIn(t, global, "shared", soloYAML("lead.md"), map[string]string{"lead.md": "GLOBAL SHARED"})
	repoDir := filepath.Join(repo, ".agent", "presets")
	writePresetIn(t, repoDir, "shared", soloYAML("lead.md"), map[string]string{"lead.md": "REPO SHARED"})
	// a repo-only preset.
	writePresetIn(t, repoDir, "local", soloYAML(""), nil)

	// Load: a global-only preset loads, and its prompt file resolves under the global folder.
	p, err := Load(repo, global, "orch")
	if err != nil {
		t.Fatalf("global-only preset should load: %v", err)
	}
	if p.LeadPromptText != "GLOBAL LEAD" {
		t.Errorf("global preset prompt = %q, want the file under the global folder", p.LeadPromptText)
	}
	if p.Dir != filepath.Join(global, "orch") {
		t.Errorf("global preset Dir = %q, want it under the global root", p.Dir)
	}

	// Repo shadows a same-named global preset (repo wins wholesale).
	sh, err := Load(repo, global, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if sh.LeadPromptText != "REPO SHARED" {
		t.Errorf("shared should resolve to the REPO copy, got %q", sh.LeadPromptText)
	}

	// List: union across roots, deduped (one "shared"), sorted.
	got := List(repo, global)
	want := []string{"local", "orch", "shared"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("List = %v, want %v", got, want)
	}

	// Path resolves to the winning root; Origin marks a global-sourced name only.
	if Path(repo, global, "orch") != filepath.Join(global, "orch", "preset.yaml") {
		t.Errorf("Path(orch) = %q, want the global path", Path(repo, global, "orch"))
	}
	if Path(repo, global, "shared") != filepath.Join(repoDir, "shared", "preset.yaml") {
		t.Errorf("Path(shared) = %q, want the repo path (repo wins)", Path(repo, global, "shared"))
	}
	if !Origin(repo, global, "orch") {
		t.Error("orch is global-sourced — Origin should be true")
	}
	if Origin(repo, global, "shared") {
		t.Error("shared is shadowed by the repo — Origin should be false (repo wins)")
	}
	if Origin(repo, global, "local") {
		t.Error("local is repo-only — Origin should be false")
	}

	// A name in NEITHER root errors and names both searched locations.
	_, err = Load(repo, global, "ghost")
	if err == nil {
		t.Fatal("a name in neither root should error")
	}
	for _, loc := range []string{repoDir, global} {
		if !strings.Contains(err.Error(), loc) {
			t.Errorf("missing-preset error should name %q:\n%v", loc, err)
		}
	}

	// globalDir == "" is repo-only: the global-only preset is invisible, single-repo unchanged.
	if names := List(repo, ""); strings.Join(names, ",") != "local,shared" {
		t.Errorf("repo-only List = %v, want [local shared]", names)
	}
	if _, err := Load(repo, "", "orch"); err == nil {
		t.Error("repo-only Load must not find a global preset")
	}
}
