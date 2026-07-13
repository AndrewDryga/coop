// Package preset loads YAML orchestration presets: a runtime recipe naming the lead
// agent and a set of roles (native subagent / read-only consult / write-capable
// delegate), each with its model, credentials, routing hints, and optional Markdown
// prompt material. A preset is DISTINCT from a credential: credentials are
// accounts/logins/rate-limit slots (stored per agent); a preset is the orchestration
// recipe that says which agent+model+credential plays which role. The package is pure
// (files + text only); the cli applies a preset's selections and box.Run mounts the
// generated contracts and wrappers.
package preset

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"gopkg.in/yaml.v3"
)

// Dir is the repo-relative home of presets: .agent/presets/<name>/preset.yaml.
const Dir = ".agent/presets"

// Role modes: how the lead reaches a role.
const (
	ModeNative   = "native"   // a coop-generated Claude subagent (.claude/agents/) in the lead's session
	ModeConsult  = "consult"  // a read-only peer via coop-consult
	ModeDelegate = "delegate" // a write-capable delegate via coop-delegate
)

// Role is one named role in a preset. A role has no credentials — it runs on its agent's
// default (marked) account and never rotates; only the LEAD rotates (see LeadLadder).
type Role struct {
	Name       string
	Mode       string   // native | consult | delegate
	Agent      string   // known agent
	Model      string   // optional model id ("" = the agent's own default)
	Effort     string   // optional reasoning-effort level ("" = the agent's own default)
	When       []string // routing hints injected into the lead contract
	Subagent   string   // native only, OPTIONAL: reference an existing subagent; empty ⇒ coop generates coop-<Name>
	PromptText string   // roles/<name>.md content, appended to the generated contract
}

// Preset is a loaded, validated orchestration preset.
type Preset struct {
	Name string
	Dir  string // the preset folder on the host (for docs/errors)

	LeadAgent string
	// LeadLadder is the lead's fallback ladder: whole targets, in order. A rung with no
	// accounts fans out across all signed-in accounts at loop start; a pinned one runs those
	// accounts only. The ladder MAY be cross-provider — the loop rotates across agents. The
	// loop rotates the expansion (expandLadder) on rate limits; a single non-loop run uses the
	// first entry. Empty = the agent's default model, all accounts.
	LeadLadder     []agents.Target
	LeadPromptText string // lead.md content, appended after the generated block

	Roles []Role // sorted by name for deterministic contracts
}

// LeadModel returns the lead's primary model — the first ladder entry's model, or "" when
// no models are declared (the agent's default resolves). Used by the generated contract and
// `coop presets`.
func (p *Preset) LeadModel() string {
	if len(p.LeadLadder) == 0 {
		return ""
	}
	return p.LeadLadder[0].Model
}

// LeadEffort returns the lead's primary reasoning effort — the first ladder entry's effort, or
// "" when none is declared. Used by the generated contract and applyPreset.
func (p *Preset) LeadEffort() string {
	if len(p.LeadLadder) == 0 {
		return ""
	}
	return p.LeadLadder[0].Effort
}

// leadLadder parses the lead's agent: node — a TARGET (scalar "claude:opus@work") or a target
// LADDER (sequence [claude:fable, claude:opus@work]) — into the lead provider (the first rung's)
// and the ladder itself, entries kept whole (expandLadder fans a rung's account list out at run
// time, against what's actually signed in). The ladder MAY be cross-provider
// ([claude:opus, codex:gpt-5]) — the loop rotates across agents. A single bare-lead entry (no
// model, no effort, no account) collapses to the empty ladder (the agent's default model, all
// accounts).
func leadLadder(node *yaml.Node) (provider string, ladder []agents.Target, err error) {
	var raw []string
	switch node.Kind {
	case yaml.ScalarNode:
		raw = []string{node.Value}
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			return "", nil, fmt.Errorf("is an empty list — name at least one target, or write a single one")
		}
		for _, c := range node.Content {
			raw = append(raw, c.Value)
		}
	case 0: // absent
		return "", nil, fmt.Errorf("is required — a target: provider[:model][@account] (e.g. %s or %s:<model>)", agents.Names()[0], agents.Names()[0])
	default:
		return "", nil, fmt.Errorf("must be a target (claude:opus@work) or a list of targets, not a map")
	}
	for i, s := range raw {
		t, perr := agents.ParseTarget(s)
		if perr != nil {
			return "", nil, fmt.Errorf("[%d] %v", i, perr)
		}
		if provider == "" {
			provider = t.Provider // the lead = the first rung's provider
		}
		ladder = append(ladder, t)
	}
	// A single bare-lead entry (no model, no effort, no account) is "default model, all
	// accounts" — the empty ladder, identical to the pre-unification absent models:.
	if len(ladder) == 1 && ladder[0].Model == "" && ladder[0].Effort == "" && len(ladder[0].Accounts) == 0 {
		ladder = nil
	}
	return provider, ladder, nil
}

// roleName limits role names to env-safe tokens: the delegate wrapper turns a role
// name into COOP_DELEGATE_<NAME>_* environment variables.
var roleName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// yaml decode targets. Model fields are pointers so an explicitly-empty value
// ("model: ") is distinguishable from an absent one and can error clearly.
type yamlPreset struct {
	Lead  yamlLead            `yaml:"lead"`
	Roles map[string]yamlRole `yaml:"roles"`
}

type yamlLead struct {
	Agent  yaml.Node `yaml:"agent"` // a TARGET or a target-LADDER, cross-provider ok
	Prompt string    `yaml:"prompt"`
}

type yamlRole struct {
	Mode string `yaml:"mode"`
	// Agent is a TARGET: provider[:model] (the model rides here; no @account). Decoded as a
	// node so a LIST gets a purposeful "one target per role" error, not a raw yaml type error.
	Agent      yaml.Node `yaml:"agent"`
	When       []string  `yaml:"when"`
	Prompt     string    `yaml:"prompt"`
	Subagent   string    `yaml:"subagent"`
	Commit     string    `yaml:"commit"`
	Concurrent string    `yaml:"concurrent"`
	// Not implemented in v1 — they imply enforcement that doesn't exist yet, so
	// setting them must fail loud instead of silently granting nothing.
	Permissions any `yaml:"permissions"`
	WritePaths  any `yaml:"write_paths"`
	DenyPaths   any `yaml:"deny_paths"`
}

// roots returns the preset search roots in precedence order: the repo's
// .agent/presets/ first, then the per-user global dir when non-empty. globalDir
// == "" means repo-only, so every single-repo run is byte-identical to before.
func roots(repo, globalDir string) []string {
	rs := []string{filepath.Join(repo, filepath.FromSlash(Dir))}
	if globalDir != "" {
		rs = append(rs, globalDir)
	}
	return rs
}

// Path returns the resolved preset.yaml for name: the first search root that holds
// it (repo wins over global), else the repo path — so an absent-preset message still
// points at the conventional .agent/presets/ spot.
func Path(repo, globalDir, name string) string {
	rs := roots(repo, globalDir)
	for _, root := range rs {
		p := filepath.Join(root, name, "preset.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(rs[0], name, "preset.yaml")
}

// Origin reports whether name resolves to the global root (true) rather than the
// repo (false) — the lister marks a global-sourced preset. A repo preset shadowing a
// same-named global one is "repo" (repo wins), so it goes unmarked.
func Origin(repo, globalDir, name string) (global bool) {
	repoPath := filepath.Join(roots(repo, "")[0], name, "preset.yaml")
	if _, err := os.Stat(repoPath); err == nil {
		return false
	}
	return globalDir != ""
}

// List returns the names of every preset folder across the search roots (a directory
// holding a preset.yaml), deduped with the repo winning a name collision, sorted. It
// doesn't validate them — the lister must show a broken preset so it can be fixed, not
// hide it.
func List(repo, globalDir string) []string {
	seen := map[string]bool{}
	var names []string
	for _, root := range roots(repo, globalDir) {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue // repo iterated first, so a repo name shadows the global one
			}
			if _, err := os.Stat(filepath.Join(root, e.Name(), "preset.yaml")); err == nil {
				seen[e.Name()] = true
				names = append(names, e.Name())
			}
		}
	}
	sort.Strings(names)
	return names
}

// ValidName reports whether name can be a preset: a single folder-name segment, not a
// verb of the presets command itself (a preset named "init" could never be shown).
func ValidName(name string) bool {
	switch name {
	case "", ".", "..", "init", "ls":
		return false
	}
	return !strings.ContainsAny(name, "/\\") && !strings.HasPrefix(name, "-")
}

// Load reads and validates a named preset's preset.yaml — the repo's .agent/presets/
// first, then the global dir (globalDir == "" = repo-only) — loading any referenced
// Markdown prompt files (they resolve relative to the folder that won, so a global
// preset's roles/*.md resolve under the global folder). Every error names the preset
// and what to fix.
func Load(repo, globalDir, name string) (*Preset, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid preset name %q — a preset is a folder name under %s/", name, Dir)
	}
	path := Path(repo, globalDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no preset %q — expected it under %s (create the folder with a preset.yaml; see 'coop help presets')", name, strings.Join(roots(repo, globalDir), " or "))
	}
	var y yamlPreset
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&y); err != nil {
		return nil, fmt.Errorf("preset %s: malformed YAML: %v", name, err)
	}

	p := &Preset{Name: name, Dir: filepath.Dir(path)}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("preset %s: %s", name, fmt.Sprintf(format, a...))
	}

	// Lead. agent: is a TARGET or a target ladder; its model+account fold in.
	// LeadAgent is the provider; LeadLadder the ladder.
	leadAgent, ladder, err := leadLadder(&y.Lead.Agent)
	if err != nil {
		return nil, bad("lead.agent: %v", err)
	}
	p.LeadAgent, p.LeadLadder = leadAgent, ladder
	if p.LeadPromptText, err = promptText(p.Dir, y.Lead.Prompt); err != nil {
		return nil, bad("lead.prompt: %v", err)
	}

	// Roles, in sorted order so generated contracts are deterministic.
	names := make([]string, 0, len(y.Roles))
	for n := range y.Roles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r, err := loadRole(p.Dir, n, y.Roles[n])
		if err != nil {
			return nil, bad("%v", err)
		}
		p.Roles = append(p.Roles, r)
	}
	return p, nil
}

func loadRole(dir, name string, y yamlRole) (Role, error) {
	r := Role{Name: name, When: y.When}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("role %s: %s", name, fmt.Sprintf(format, a...))
	}
	if !roleName.MatchString(name) {
		return r, fmt.Errorf("role name %q — use lowercase letters, digits, and dashes (it becomes the coop-delegate argument and an env var)", name)
	}
	switch y.Mode {
	case ModeNative, ModeConsult, ModeDelegate:
		r.Mode = y.Mode
	case "":
		return r, bad("mode is required (native, consult, or delegate)")
	default:
		return r, bad("mode %q is not one of native, consult, delegate", y.Mode)
	}
	switch y.Agent.Kind {
	case yaml.ScalarNode:
	case 0:
		return r, bad("agent is required — a target: provider[:model] (e.g. %s or %s:<model>)", agents.Names()[0], agents.Names()[0])
	case yaml.SequenceNode:
		// Not a parse gap — a deliberate boundary: nothing rotates a role (a consult/delegate is
		// one wrapper call; only the lead's loop rotates), so accepting a ladder here would be
		// dead config that looks like failover.
		return r, bad("agent: is a list — a role runs ONE target; fallback ladders belong to the lead (lead.agent)")
	default:
		return r, bad("agent: must be a target (provider[:model]), not a map")
	}
	if y.Agent.Value == "" {
		return r, bad("agent is required — a target: provider[:model] (e.g. %s or %s:<model>)", agents.Names()[0], agents.Names()[0])
	}
	// agent: is a TARGET — provider[:model]. The model rides here; a role runs its agent's DEFAULT
	// account, so an @account is rejected (only the lead rotates accounts).
	t, terr := agents.ParseTarget(y.Agent.Value)
	if terr != nil {
		return r, bad("agent %q: %v", y.Agent.Value, terr)
	}
	if len(t.Accounts) > 0 {
		return r, bad("agent %q pins an account — a role runs its agent's default account (only the lead rotates); drop the @account", y.Agent.Value)
	}
	r.Agent, r.Model, r.Effort = t.Provider, t.Model, t.Effort
	if y.Permissions != nil || y.WritePaths != nil || y.DenyPaths != nil {
		return r, bad("permissions/write_paths/deny_paths are not supported — coop can't enforce path-level permissions yet, so declaring them would only pretend to")
	}

	// Mode-specific shape.
	switch r.Mode {
	case ModeNative:
		if r.Agent != "claude" {
			return r, bad("mode: native generates a Claude subagent (.claude/agents/) — its agent must be claude, not %s; a codex/gemini role uses consult or delegate", r.Agent)
		}
		// subagent is OPTIONAL: set = reference that existing .claude/agents/ subagent;
		// empty = coop generates coop-<role> in the box from this role (model/when/prompt).
		r.Subagent = y.Subagent
	default:
		if y.Subagent != "" {
			return r, bad("subagent only applies to mode: native")
		}
	}
	switch r.Mode {
	case ModeDelegate:
		if y.Commit != "" && y.Commit != "never" {
			return r, bad("commit: %q — only 'never' is supported (the delegate edits, the lead commits)", y.Commit)
		}
		if y.Concurrent != "" && y.Concurrent != "never" {
			return r, bad("concurrent: %q — only 'never' is supported (delegate runs are serialized)", y.Concurrent)
		}
	default:
		if y.Commit != "" || y.Concurrent != "" {
			return r, bad("commit/concurrent only apply to mode: delegate")
		}
	}

	var err error
	if r.PromptText, err = promptText(dir, y.Prompt); err != nil {
		return r, bad("prompt: %v", err)
	}
	return r, nil
}

// promptText loads an optional Markdown prompt file (relative to the preset folder).
// A declared file that doesn't exist is an error — a silent skip would quietly drop
// the user's prompt material.
func promptText(dir, rel string) (string, error) {
	if rel == "" {
		return "", nil
	}
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return "", fmt.Errorf("%q must be a relative path inside the preset folder", rel)
	}
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return "", fmt.Errorf("declared prompt file %q does not exist in the preset folder", rel)
	}
	return strings.TrimSpace(string(data)), nil
}

// CrossProvider reports whether the lead ladder spans providers — a rung on a different
// provider than the lead (the first rung). The loop EMBRACES this (rotation swaps the agent
// per rung); single-lead surfaces don't: fusion errors (one governor for the whole council)
// and ACP filters to the lead's own rungs (its failover env carries no provider).
func (p *Preset) CrossProvider() bool {
	for _, t := range p.LeadLadder {
		if t.Provider != p.LeadAgent {
			return true
		}
	}
	return false
}

// HasConsult reports whether any role is a read-only consult peer (mount coop-consult).
func (p *Preset) HasConsult() bool { return p.hasMode(ModeConsult) }

// HasDelegate reports whether any role is a write-capable delegate (mount coop-delegate).
func (p *Preset) HasDelegate() bool { return p.hasMode(ModeDelegate) }

func (p *Preset) hasMode(mode string) bool {
	for _, r := range p.Roles {
		if r.Mode == mode {
			return true
		}
	}
	return false
}

// Delegates returns the delegate roles, in name order.
func (p *Preset) Delegates() []Role {
	var out []Role
	for _, r := range p.Roles {
		if r.Mode == ModeDelegate {
			out = append(out, r)
		}
	}
	return out
}

// Consults returns the explicit consult roles, in name order. Each is wired role-addressed
// (`coop-consult <role>` via COOP_CONSULT_<ROLE>_* env) so it runs its agent on the role's
// model with the role's persona — natives degraded under a non-Claude lead wire the same way
// (DegradedNativeRoles).
func (p *Preset) Consults() []Role {
	var out []Role
	for _, r := range p.Roles {
		if r.Mode == ModeConsult {
			out = append(out, r)
		}
	}
	return out
}

// RoleAgents returns the distinct agents of consult and delegate roles — the ones
// whose credentials must be reachable from the lead's box. Native roles run inside
// the lead's own session, so they add nothing to the credential scope.
func (p *Preset) RoleAgents() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range p.Roles {
		if r.Mode == ModeNative || seen[r.Agent] {
			continue
		}
		seen[r.Agent] = true
		out = append(out, r.Agent)
	}
	return out
}
