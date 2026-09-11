// Package preset loads YAML orchestration presets: a runtime recipe naming the lead
// target and a set of roles (native subagent / read-only consult / write-capable
// delegate), each with a target selection, routing hints, and optional Markdown prompt
// material. Consult/delegate roles may carry target/model fallback ladders; a native
// role has one target. A preset is distinct from a credential: credentials are
// host-stored accounts/logins/rate-limit slots, while role targets deliberately use
// each provider's default account. The package is pure (files + text only); the cli
// applies a preset's selections and box.Run mounts the generated contracts and wrappers.
package preset

import (
	"errors"
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
	ModeNative   = "native"   // a provider-native subagent in the lead's session
	ModeConsult  = "consult"  // a read-only peer via coop-consult
	ModeDelegate = "delegate" // a write-capable delegate via coop-delegate
)

// Role is one named role in a preset. Consult and delegate roles may carry ordered
// fallback targets; every target uses that provider's default account.
type Role struct {
	Name       string
	Mode       string // native | consult | delegate
	Targets    []agents.Target
	When       []string // routing hints injected into the lead contract
	Subagent   string   // native only, OPTIONAL: reference an existing subagent; empty ⇒ coop generates coop-<Name>
	PromptText string   // roles/<name>.md content, appended to the generated contract
	// PromptPath is the configured prompt: as written, set only once its file loaded — so
	// `coop presets <name>` can show the exact file execution appends and never a path it
	// didn't read. An empty prompt: stays empty (the role uses the generated contract alone).
	PromptPath string
}

// Preset is a loaded, validated orchestration preset.
type Preset struct {
	Name string
	Dir  string // the preset folder on the host (for docs/errors)

	// LeadTargets is the lead's fallback ladder: whole targets, in order. A rung with no
	// accounts fans out across all signed-in accounts at loop start; a pinned one runs those
	// accounts only. The ladder MAY be cross-provider — the loop rotates across agents. The
	// loop rotates the expansion (expandLadder) on rate limits; a single non-loop run uses the
	// first entry. Load always returns at least one target; a bare provider means its default
	// model across all accounts.
	LeadTargets    []agents.Target
	LeadPromptText string // lead.md content, appended after the generated block
	LeadPromptPath string // the configured lead prompt:, set only once its file loaded (see Role.PromptPath)

	Roles []Role // in the order preset.yaml declares them (see roleOrder)
}

// Lead returns the preset's primary target. Loaded presets always have one; the zero value keeps
// hand-built internal values safe to inspect without creating a second stored representation.
func (p *Preset) Lead() agents.Target {
	if p == nil || len(p.LeadTargets) == 0 {
		return agents.Target{}
	}
	return p.LeadTargets[0]
}

// LeadModel returns the lead's primary model — the first ladder entry's model, or "" when
// no models are declared (the agent's default resolves). Used by the generated contract and
// `coop presets`.
func (p *Preset) LeadModel() string {
	return p.Lead().Model
}

// LeadEffort returns the lead's primary reasoning effort — the first ladder entry's effort, or
// "" when none is declared. Used by the generated contract and applyPreset.
func (p *Preset) LeadEffort() string {
	return p.Lead().Effort
}

// leadTargets parses the lead's agent: node — a TARGET (scalar "claude:opus@work") or a target
// LADDER (sequence [claude:fable, claude:opus@work]) — into whole targets. expandLadder fans a
// target's account list out at run time against what's actually signed in. The ladder MAY be
// cross-provider
// ([claude:opus, codex:gpt-5]) — the loop rotates across agents. A bare provider remains a real
// target whose empty model/effort and accounts mean provider defaults and account fan-out.
func leadTargets(node *yaml.Node) ([]agents.Target, []string) {
	var raw []string
	switch node.Kind {
	case yaml.ScalarNode:
		raw = []string{node.Value}
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			return nil, []string{"The lead's agent list is empty.", "Name at least one target, or write a single one."}
		}
		for _, c := range node.Content {
			raw = append(raw, c.Value)
		}
	case 0: // absent
		return nil, []string{"The lead needs an agent.", fmt.Sprintf("Write agent: %s, optionally %s:<model>.", agents.Names()[0], agents.Names()[0])}
	default:
		return nil, []string{"The lead's agent must be one target or a list of targets, not a map."}
	}
	targets := make([]agents.Target, 0, len(raw))
	for _, s := range raw {
		t, perr := agents.ParseTarget(s)
		if perr != nil {
			return nil, targetDetail("The lead's agent", s, perr)
		}
		targets = append(targets, t)
	}
	return targets, nil
}

// targetDetail turns a rejected target inside a preset into the loader's own sentences: what the
// file says, then the parser's constraint — which internal/agent already states as a sentence.
func targetDetail(what, raw string, err error) []string {
	detail := []string{fmt.Sprintf("%s %q is not a valid target.", what, agents.DisplayTarget(raw))}
	var te *agents.TargetError
	if errors.As(err, &te) && te.Cause != "" {
		return append(detail, strings.Split(te.Cause, "\n")...)
	}
	return append(detail, err.Error())
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
	// Agent is a TARGET or, for consult/delegate, a fallback ladder. Accounts are not allowed.
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

// ErrNotFound marks a name with no preset.yaml under any search root — a different answer from a
// preset that exists but will not load, which a caller shows the problem for instead of a typo.
var ErrNotFound = errors.New("no such preset")

// LoadError is a preset that would not load, as DATA: the file a reader must open, and the problem
// as complete sentences — the first says what is wrong, a following one says how to fix it. The
// surface that owns the terminal prints them under its own headline (internal/preset renders
// nothing itself); Error() flattens the same facts onto one line for a log.
type LoadError struct {
	Name     string
	Path     string   // the preset.yaml this is about
	Detail   []string // sentences, first one first
	NotFound bool     // no such preset, rather than a broken one
}

func (e *LoadError) Error() string {
	return "preset " + e.Name + ": " + strings.Join(e.Detail, " ")
}

func (e *LoadError) Is(target error) bool { return target == ErrNotFound && e.NotFound }

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
		return nil, &LoadError{Name: name, NotFound: true, Detail: []string{
			fmt.Sprintf("%q is not a usable preset name.", name),
			fmt.Sprintf("A preset is a folder name under %s/.", Dir),
		}}
	}
	path := Path(repo, globalDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &LoadError{Name: name, Path: path, NotFound: true, Detail: []string{
			fmt.Sprintf("No preset %q exists.", name),
			fmt.Sprintf("Coop looked under %s.", strings.Join(roots(repo, globalDir), " and ")),
		}}
	}
	dir := filepath.Dir(path)
	return loadPreset(name, dir, data, func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, rel))
	})
}

// Scaffold shares validation while reading from its pinned, unpublished bundle.
func loadPreset(name, dir string, data []byte, readFile func(string) ([]byte, error)) (*Preset, error) {
	bad := func(detail ...string) (*Preset, error) {
		return nil, &LoadError{Name: name, Path: filepath.Join(dir, "preset.yaml"), Detail: detail}
	}
	var y yamlPreset
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&y); err != nil {
		return bad("Coop could not read this file as YAML.", strings.TrimSpace(err.Error()))
	}

	p := &Preset{Name: name, Dir: dir}

	// Lead. agent: is a TARGET or a target ladder; its model+account stay on the target.
	targets, detail := leadTargets(&y.Lead.Agent)
	if detail != nil {
		return bad(detail...)
	}
	p.LeadTargets = targets
	text, detail := promptText(y.Lead.Prompt, readFile)
	if detail != nil {
		return bad(detail...)
	}
	p.LeadPromptText = text
	p.LeadPromptPath = y.Lead.Prompt

	// Roles in the order the file declares them — deterministic, and the order its author chose.
	for _, n := range roleOrder(data, y.Roles) {
		r, detail := loadRole(n, y.Roles[n], readFile)
		if detail != nil {
			return bad(detail...)
		}
		p.Roles = append(p.Roles, r)
	}
	return p, nil
}

// roleOrder lists a preset's roles in the order its preset.yaml declares them. A person reads back
// the file they wrote — the role they put first is the one they reach for most — so the detail page
// and the lead's contract keep that order instead of alphabetising it away. A Go map cannot hold
// it, and yaml.Node.Decode would drop the strict decode's KnownFields check, so the order comes
// from a second parse of the same bytes the first one already proved well-formed; an unrecognized
// shape falls back to sorted names, because the one thing this must never be is arbitrary.
func roleOrder(data []byte, roles map[string]yamlRole) []string {
	names := make([]string, 0, len(roles))
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err == nil && len(doc.Content) == 1 {
		root := doc.Content[0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value != "roles" {
				continue
			}
			declared := root.Content[i+1].Content
			for j := 0; j+1 < len(declared); j += 2 {
				if _, ok := roles[declared[j].Value]; ok {
					names = append(names, declared[j].Value)
				}
			}
		}
	}
	if len(names) != len(roles) {
		names = names[:0]
		for n := range roles {
			names = append(names, n)
		}
		sort.Strings(names)
	}
	return names
}

func loadRole(name string, y yamlRole, readFile func(string) ([]byte, error)) (Role, []string) {
	r := Role{Name: name, When: y.When}
	role := fmt.Sprintf("Role %q", name)
	bad := func(detail ...string) (Role, []string) { return r, detail }
	if !roleName.MatchString(name) {
		return bad(fmt.Sprintf("Role name %q is not usable.", name),
			"Use lowercase letters, digits, and dashes: the name becomes a coop-delegate argument and an environment variable.")
	}
	modes := "Choose native, consult, or delegate."
	switch y.Mode {
	case ModeNative, ModeConsult, ModeDelegate:
		r.Mode = y.Mode
	case "":
		return bad(role+" needs a mode.", modes)
	default:
		return bad(fmt.Sprintf("%s has an unknown mode %q.", role, y.Mode), modes)
	}
	var rawTargets []string
	switch y.Agent.Kind {
	case yaml.ScalarNode:
		rawTargets = []string{y.Agent.Value}
	case 0:
		return bad(role+" needs an agent.", fmt.Sprintf("Write agent: %s, optionally %s:<model>.", agents.Names()[0], agents.Names()[0]))
	case yaml.SequenceNode:
		if len(y.Agent.Content) == 0 {
			return bad(role+" has an empty agent list.", "Name at least one target, or write a single one.")
		}
		if r.Mode == ModeNative {
			return bad(role+" is native, so it takes one agent.",
				"Native subagents have no fallback; use mode: consult or delegate for a ladder.")
		}
		for i, node := range y.Agent.Content {
			if node.Kind != yaml.ScalarNode {
				return bad(fmt.Sprintf("%s agent[%d] must be a target, not a map or list.", role, i))
			}
			rawTargets = append(rawTargets, node.Value)
		}
	default:
		return bad(role + " agent must be one target or a list of targets, not a map.")
	}
	for _, raw := range rawTargets {
		t, terr := agents.ParseTarget(raw)
		if terr != nil {
			return bad(targetDetail(role+" agent", raw, terr)...)
		}
		if len(t.Accounts) > 0 {
			return bad(fmt.Sprintf("%s pins the account %q.", role, "@"+strings.Join(t.Accounts, ",")),
				"Roles use each provider's default account, so remove it.")
		}
		r.Targets = append(r.Targets, t)
	}
	if y.Permissions != nil || y.WritePaths != nil || y.DenyPaths != nil {
		return bad(role+" sets permissions, write_paths, or deny_paths.",
			"Coop cannot enforce path-level permissions yet, so declaring them would only pretend to.")
	}

	// Mode-specific shape.
	switch r.Mode {
	case ModeNative:
		ag, _ := agents.Get(r.Primary().Provider)
		support := ag.NativeSubagents()
		if support.HomeDir == "" || support.Render == nil {
			return bad(fmt.Sprintf("%s is native, but %s has no in-session subagents.", role, r.Primary().Provider),
				"Use mode: consult or delegate.")
		}
		// subagent is OPTIONAL: set = reference an adapter-native subagent; empty = coop
		// generates coop-<role> in the box from this role (model/when/prompt).
		r.Subagent = y.Subagent
	default:
		if y.Subagent != "" {
			return bad(role + " sets subagent, which only applies to mode: native.")
		}
	}
	switch r.Mode {
	case ModeDelegate:
		if y.Commit != "" && y.Commit != "never" {
			return bad(fmt.Sprintf("%s sets commit: %q.", role, y.Commit),
				`Only "never" is supported: the delegate edits, the lead commits.`)
		}
		if y.Concurrent != "" && y.Concurrent != "never" {
			return bad(fmt.Sprintf("%s sets concurrent: %q.", role, y.Concurrent),
				`Only "never" is supported: delegate runs are serialized.`)
		}
	default:
		if y.Commit != "" || y.Concurrent != "" {
			return bad(role + " sets commit or concurrent, which only apply to mode: delegate.")
		}
	}

	text, detail := promptText(y.Prompt, readFile)
	if detail != nil {
		return bad(detail...)
	}
	r.PromptText = text
	r.PromptPath = y.Prompt
	return r, nil
}

// promptText loads an optional Markdown prompt file (relative to the preset folder).
// A declared file that doesn't exist is an error — a silent skip would quietly drop
// the user's prompt material.
func promptText(rel string, readFile func(string) ([]byte, error)) (string, []string) {
	if rel == "" {
		return "", nil
	}
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return "", []string{fmt.Sprintf("Prompt file %q must be inside the preset folder.", rel)}
	}
	data, err := readFile(filepath.FromSlash(rel))
	if err != nil {
		return "", []string{fmt.Sprintf("Prompt file %q was not found.", rel)}
	}
	return strings.TrimSpace(string(data)), nil
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

// ConsultRoles returns every role that the effective lead invokes through coop-consult, in
// preset order: explicit consult roles under every lead, plus native roles degraded when the
// effective lead cannot host generated subagents. This is the canonical wrapper/instruction view.
func (p *Preset) ConsultRoles(lead string) []Role {
	var out []Role
	for _, r := range p.Roles {
		if r.Mode == ModeConsult || (r.Mode == ModeNative && !nativeRoleUsable(&r, lead)) {
			out = append(out, r)
		}
	}
	return out
}

// RunnableRoleAgents returns the distinct providers whose credentials the effective lead needs
// for consult and delegate roles. A native role contributes only when it degrades to a consult.
func (p *Preset) RunnableRoleAgents(lead string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range p.Roles {
		if r.Mode != ModeConsult && r.Mode != ModeDelegate && !(r.Mode == ModeNative && !nativeRoleUsable(&r, lead)) {
			continue
		}
		for _, target := range r.Targets {
			if seen[target.Provider] {
				continue
			}
			seen[target.Provider] = true
			out = append(out, target.Provider)
		}
	}
	return out
}

// Primary returns the role's first target. Loaded roles always have one; the zero value keeps
// hand-built internal values safe to inspect without a compatibility representation.
func (r Role) Primary() agents.Target {
	if len(r.Targets) == 0 {
		return agents.Target{}
	}
	return r.Targets[0]
}
