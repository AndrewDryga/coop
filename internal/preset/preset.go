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
	ModeNative   = "native"   // a native Claude subagent inside the lead's own session
	ModeConsult  = "consult"  // a read-only peer via coop-consult
	ModeDelegate = "delegate" // a write-capable delegate via coop-delegate
)

// Role is one named role in a preset.
type Role struct {
	Name        string
	Mode        string   // native | consult | delegate
	Agent       string   // known agent
	Model       string   // optional model id ("" = the agent's own default chain)
	Credentials []string // optional credential names (accounts) this role runs on
	When        []string // routing hints injected into the lead contract
	Subagent    string   // native-only: the Claude subagent name (e.g. deep-reasoner)
	PromptText  string   // roles/<name>.md content, appended to the generated contract
}

// Preset is a loaded, validated orchestration preset.
type Preset struct {
	Name string
	Dir  string // the preset folder on the host (for docs/errors)

	LeadAgent       string
	LeadModel       string
	LeadCredentials []string
	LeadPromptText  string // lead.md content, appended after the generated block

	Roles []Role // sorted by name for deterministic contracts
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
	Agent       string    `yaml:"agent"`
	Model       *string   `yaml:"model"`
	Credentials *[]string `yaml:"credentials"`
	Credential  *string   `yaml:"credential"` // rejected with a pointer to the plural
	Prompt      string    `yaml:"prompt"`
}

type yamlRole struct {
	Mode        string    `yaml:"mode"`
	Agent       string    `yaml:"agent"`
	Model       *string   `yaml:"model"`
	Credentials *[]string `yaml:"credentials"`
	Credential  *string   `yaml:"credential"` // rejected with a pointer to the plural
	When        []string  `yaml:"when"`
	Prompt      string    `yaml:"prompt"`
	Subagent    string    `yaml:"subagent"`
	Commit      string    `yaml:"commit"`
	Concurrent  string    `yaml:"concurrent"`
	// Not implemented in v1 — they imply enforcement that doesn't exist yet, so
	// setting them must fail loud instead of silently granting nothing.
	Permissions any `yaml:"permissions"`
	WritePaths  any `yaml:"write_paths"`
	DenyPaths   any `yaml:"deny_paths"`
}

// Path returns the preset.yaml path for a named preset under repo.
func Path(repo, name string) string {
	return filepath.Join(repo, filepath.FromSlash(Dir), name, "preset.yaml")
}

// List returns the names of every preset folder under repo (a directory in
// .agent/presets/ holding a preset.yaml), sorted. It doesn't validate them —
// the lister must show a broken preset so it can be fixed, not hide it.
func List(repo string) []string {
	entries, err := os.ReadDir(filepath.Join(repo, filepath.FromSlash(Dir)))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(Path(repo, e.Name())); err == nil {
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

// Load reads and validates .agent/presets/<name>/preset.yaml under repo, loading any
// referenced Markdown prompt files. Every error names the preset and what to fix.
func Load(repo, name string) (*Preset, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid preset name %q — a preset is a folder name under %s/", name, Dir)
	}
	path := Path(repo, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no preset %q — expected %s (create the folder with a preset.yaml; see 'coop help presets')", name, filepath.Join(Dir, name, "preset.yaml"))
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

	// Lead.
	if y.Lead.Agent == "" {
		return nil, bad("lead.agent is required (one of %s)", strings.Join(agents.Names(), ", "))
	}
	if !agents.Valid(y.Lead.Agent) {
		return nil, bad("lead.agent %q is not a known agent (use %s)", y.Lead.Agent, strings.Join(agents.Names(), ", "))
	}
	p.LeadAgent = y.Lead.Agent
	if y.Lead.Credential != nil {
		return nil, bad("lead.credential — use the plural list form: credentials: [name, ...]")
	}
	if y.Lead.Model != nil {
		if *y.Lead.Model == "" {
			return nil, bad("lead.model is empty — set a model id or drop the key")
		}
		p.LeadModel = *y.Lead.Model
	}
	if y.Lead.Credentials != nil {
		if p.LeadCredentials, err = credentialList(*y.Lead.Credentials); err != nil {
			return nil, bad("lead.credentials %v", err)
		}
	}
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
	if y.Agent == "" {
		return r, bad("agent is required (one of %s)", strings.Join(agents.Names(), ", "))
	}
	if !agents.Valid(y.Agent) {
		return r, bad("agent %q is not a known agent (use %s)", y.Agent, strings.Join(agents.Names(), ", "))
	}
	r.Agent = y.Agent
	if y.Credential != nil {
		return r, bad("credential — use the plural list form: credentials: [name, ...]")
	}
	if y.Model != nil {
		if *y.Model == "" {
			return r, bad("model is empty — set a model id or drop the key")
		}
		r.Model = *y.Model
	}
	if y.Credentials != nil {
		var err error
		if r.Credentials, err = credentialList(*y.Credentials); err != nil {
			return r, bad("credentials %v", err)
		}
	}
	if y.Permissions != nil || y.WritePaths != nil || y.DenyPaths != nil {
		return r, bad("permissions/write_paths/deny_paths are not supported — coop can't enforce path-level permissions yet, so declaring them would only pretend to")
	}

	// Mode-specific shape.
	switch r.Mode {
	case ModeNative:
		if y.Subagent == "" {
			return r, bad("mode: native needs subagent: <name> (the Claude subagent to invoke)")
		}
		if r.Agent != "claude" {
			return r, bad("mode: native is Claude subagents — agent must be claude, not %s", r.Agent)
		}
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

// credentialList validates a credentials: list — present means non-empty, and every
// name a single path-safe segment (they become profile directory names).
func credentialList(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("is empty — list at least one credential name, or drop the key")
	}
	for _, n := range names {
		if n == "" || strings.ContainsAny(n, "/\\") || n == "." || n == ".." || strings.HasPrefix(n, "-") {
			return nil, fmt.Errorf("has invalid name %q — use a single segment (no '/', '..', or leading '-')", n)
		}
	}
	return names, nil
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
