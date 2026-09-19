package sessionsvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"gopkg.in/yaml.v3"
)

const (
	// PolicyFileLimit bounds a session policy file, so a command that validates one before
	// handing it over reads the same amount this package will.
	PolicyFileLimit = 1 << 20
	// DefaultStopTimeout is how long the service waits for in-flight work to wind down, and the
	// budget a host's own HTTP shutdown should match.
	DefaultStopTimeout = 5 * time.Second
)

// errSessionForkUnproven marks a session whose workspace authority cannot be proved at start —
// its generation record or workspace is gone, or the fork was recreated under the same name. The
// daemon quarantines such a session (every operation on it keeps failing the live authority
// check) instead of refusing to start for everyone else; its durable history stays untouched.
var errSessionForkUnproven = errors.New("remote session workspace authority is unproven")

var errLegacySessionForkUnproven = fmt.Errorf("%w: legacy remote session has no immutable fork ownership proof", errSessionForkUnproven)

const (
	sessionPolicyVersion             = 1
	sessionPolicyMaxCompanions       = 32
	sessionPolicyMaxTargets          = 4
	sessionTargetGrammar             = "provider[:model][/effort][@credential]"
	sessionPolicyRemoteLookupTimeout = 30 * time.Second
	sessionPolicyRemoteFetchTimeout  = 2 * time.Minute
	sessionPolicyRemoteConcurrency   = 4
	sessionPolicyMaxTurnTimeout      = 24 * time.Hour
	sessionPolicyMaxWarmIdleTimeout  = time.Hour
	sessionServiceCleanupInterval    = time.Minute
	sessionOperationStaleAfter       = 2 * time.Minute
	sessionCreateConcurrency         = 2
	runtimeCleanupBatchSize          = 2
	startupReapErrorLimit            = 8
)

// Policy is operator-owned authority for one remote session. It is intentionally small:
// repository, target, and resource bounds are not request fields.
type Policy struct {
	Name string
	// Mode is the execution mode every session of this policy runs under — normal (the
	// default, and every policy written before modes existed), readonly, or bare. It is fixed
	// at creation and bound into both digests, so an edit rotates sessions rather than widening
	// them. Readonly implies RepositoryReadOnly; bare implies no repository, no companions, and
	// no shared environment or MCP (see validateRestrictedSessionPolicy).
	Mode               agents.ExecutionMode
	Repository         string
	Remote             string
	Branch             string
	Companions         []CompanionPolicy
	Targets            []agents.Target
	OmitEnv            bool
	OmitMCP            bool
	RepositoryReadOnly bool
	Egress             EgressPolicy
	MaxTurns           int
	MaxQueuedTurns     int
	MaxQueuedBytes     int
	TurnTimeout        time.Duration
	WarmIdleTimeout    time.Duration
	MaxPatchBytes      int
}

// EgressPolicy is a session policy's network authority: the posture its boxes run under and the
// destinations the operator granted them. It is explicit operator authority — a create or turn
// request can never contribute a rule — and it is frozen into the session at creation, so a later
// edit applies to NEW sessions only. The zero value is the built-in open posture, which is exactly
// what a policy file with no `egress:` block means.
type EgressPolicy struct {
	Mode  egress.Mode   `json:"mode,omitempty"`
	Rules []egress.Rule `json:"rules,omitempty"`
	// ExportDestinations opts this session's outbound projections into concrete domain and peer
	// names. Owner-private records always carry them; the worker's projection does not, because
	// even a denied name can encode a secret.
	ExportDestinations bool `json:"export_destinations,omitempty"`
}

// resolvedMode is the posture this policy asks for. An absent block is the built-in open default,
// which is not an explicit request to widen anything.
func (e EgressPolicy) resolvedMode() egress.Mode {
	if e.Mode == "" {
		return egress.Open
	}
	return e.Mode
}

// configured reports whether the operator wrote an `egress:` block at all. A policy that did not
// must digest exactly as it did before this field existed, so sessions created by an earlier
// binary keep matching their policy.
func (e EgressPolicy) configured() bool {
	return e.Mode != "" || len(e.Rules) != 0 || e.ExportDestinations
}

// UnmarshalJSON retains the write-ahead intent format written before target ladders. Policy
// files use YAML and new JSON intents marshal Targets, so compatibility stays read-only.
func (p *Policy) UnmarshalJSON(data []byte) error {
	type policyAlias Policy
	wire := struct {
		*policyAlias
		LegacyTarget string `json:"Target"`
	}{policyAlias: (*policyAlias)(p)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if len(p.Targets) != 0 || wire.LegacyTarget == "" {
		return nil
	}
	target, err := agents.ParseTarget(wire.LegacyTarget)
	if err != nil {
		return fmt.Errorf("decode legacy session target: %w", err)
	}
	p.Targets = []agents.Target{target}
	return nil
}

type CompanionPolicy struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Remote     string `json:"remote,omitempty"`
	Branch     string `json:"branch,omitempty"`
}

type rawSessionPolicyFile struct {
	Version  int                         `yaml:"version"`
	Policies map[string]rawSessionPolicy `yaml:"policies"`
	// Storage is the optional worker storage policy. It is declared here because the decode is
	// strict: a file carrying the block would otherwise be refused outright. LoadStorageLimits
	// reads it; LoadPolicies ignores it, so the two readers share one document without either
	// owning the other's shape.
	Storage *rawSessionStorage `yaml:"storage"`
}

// rawSessionStorage is all-or-nothing on purpose. The reserve and the two watermarks are one
// ordered policy, and accepting a high watermark with no low one would defer an incoherent
// configuration to the first time the volume filled up instead of refusing it at the file.
type rawSessionStorage struct {
	ReserveBytes          int64  `yaml:"reserve_bytes"`
	HighWatermarkBytes    int64  `yaml:"high_watermark_bytes"`
	LowWatermarkBytes     int64  `yaml:"low_watermark_bytes"`
	DisposableBudgetBytes int64  `yaml:"disposable_budget_bytes"`
	ProtectedBudgetBytes  int64  `yaml:"protected_budget_bytes"`
	GraceWindow           string `yaml:"grace_window"`
	MeasureInterval       string `yaml:"measure_interval"`
	MaxReclaimPerPass     int    `yaml:"max_reclaim_per_pass"`
}

type rawSessionPolicy struct {
	Mode               string                      `yaml:"mode"`
	Repository         string                      `yaml:"repository"`
	Remote             string                      `yaml:"remote"`
	Branch             string                      `yaml:"branch"`
	Companions         []rawSessionCompanionPolicy `yaml:"companions"`
	Target             yaml.Node                   `yaml:"target"`
	ProjectEnv         *bool                       `yaml:"project_env"`
	ProjectMCP         *bool                       `yaml:"project_mcp"`
	RepositoryReadOnly bool                        `yaml:"repository_read_only"`
	Egress             *rawSessionEgressPolicy     `yaml:"egress"`
	MaxTurns           int                         `yaml:"max_turns"`
	MaxQueuedTurns     int                         `yaml:"max_queued_turns"`
	MaxQueuedBytes     int                         `yaml:"max_queued_bytes"`
	TurnTimeout        string                      `yaml:"turn_timeout"`
	WarmIdleTimeout    string                      `yaml:"warm_idle_timeout"`
	MaxPatchBytes      int                         `yaml:"max_patch_bytes"`
}

// rawSessionEgressPolicy is the file shape. `rules` uses the same grammar a repository's
// `box.egress_rules` does, so an operator writes one rule form, not two.
type rawSessionEgressPolicy struct {
	Mode               string        `yaml:"mode"`
	Rules              []egress.Rule `yaml:"rules"`
	ExportDestinations bool          `yaml:"export_destinations"`
}

type rawSessionCompanionPolicy struct {
	Name       string `yaml:"name"`
	Repository string `yaml:"repository"`
	Remote     string `yaml:"remote"`
	Branch     string `yaml:"branch"`
}

// LoadPolicies parses the strict operator policy file. A config is required when the
// caller wants credential availability checked; passing nil performs syntax/target/repository
// validation only and is useful for isolated parser tests.
func LoadPolicies(path string, cfg *config.Config) (map[string]Policy, error) {
	data, err := readSessionPolicyFile(path)
	if err != nil {
		return nil, err
	}
	return parseSessionPolicies(data, cfg)
}

// readSessionPolicyFile is the trusted-file half of LoadPolicies: the ancestry, ownership,
// permission and size checks every reader of that document must pass before parsing it.
func readSessionPolicyFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("session policy path is required")
	}
	path, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("resolve session policy path: %w", err)
	}
	if err := validateSessionPolicyAncestry(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("read session policy file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect session policy file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("session policy file is not a regular file")
	}
	if owner, ok := sessionFileOwner(info); !ok || (owner != uint64(os.Geteuid()) && owner != 0) {
		return nil, errors.New("session policy file has an untrusted owner")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("session policy file is group/world writable")
	}
	data, err := io.ReadAll(io.LimitReader(file, PolicyFileLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read session policy file: %w", err)
	}
	if len(data) > PolicyFileLimit {
		return nil, fmt.Errorf("session policy file exceeds %d bytes", PolicyFileLimit)
	}
	return data, nil
}

func sessionFileOwner(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Uid), true
}

func validateSessionPolicyAncestry(path string) error {
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect session policy ancestry: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Name the link and where it points: /tmp and a dotfile-managed ~/.config are the usual
			// culprits, and the fix is to pass the real path, not to remove the link.
			hint := ""
			if real, err := filepath.EvalSymlinks(current); err == nil {
				hint = " — pass its real path instead (--policies " + filepath.Join(real, strings.TrimPrefix(path, current)) + ")"
			}
			return fmt.Errorf("session policy path %s: %s is a symlink%s", path, current, hint)
		}
		if !info.IsDir() {
			return fmt.Errorf("session policy path %s: %s is not a directory", path, current)
		}
		owner, ownerOK := sessionFileOwner(info)
		// A trusted sticky directory such as /tmp lets others create their own entries,
		// but not replace this user's protected descendant.
		trustedSticky := info.Mode()&os.ModeSticky != 0 && ownerOK &&
			(owner == uint64(os.Geteuid()) || owner == 0)
		if info.Mode().Perm()&0o022 != 0 && !trustedSticky {
			return fmt.Errorf("session policy path %s: %s is group/world writable", path, current)
		}
		if !ownerOK {
			return fmt.Errorf("session policy path %s: the owner of %s is unavailable", path, current)
		} else if owner != uint64(os.Geteuid()) && owner != 0 {
			return fmt.Errorf("session policy path %s: %s is owned by another user", path, current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func parseSessionPolicies(data []byte, cfg *config.Config) (map[string]Policy, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawSessionPolicyFile
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode session policies: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("session policy file contains more than one YAML document")
		}
		return nil, fmt.Errorf("decode trailing session policy document: %w", err)
	}
	if raw.Version != sessionPolicyVersion {
		return nil, fmt.Errorf("session policy version must be %d", sessionPolicyVersion)
	}
	if len(raw.Policies) == 0 {
		return nil, errors.New("session policy file must define at least one policy")
	}
	policies := make(map[string]Policy, len(raw.Policies))
	for name, item := range raw.Policies {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("session policy names must be nonempty")
		}
		policy, err := validateSessionPolicy(name, item, cfg)
		if err != nil {
			return nil, fmt.Errorf("policy %q: %w", name, err)
		}
		policies[name] = policy
	}
	return policies, nil
}

func validateSessionPolicy(name string, raw rawSessionPolicy, cfg *config.Config) (Policy, error) {
	mode := agents.ModeNormal
	if raw.Mode != "" {
		var err error
		if mode, err = agents.ParseExecutionMode(raw.Mode); err != nil {
			return Policy{}, fmt.Errorf("mode: %w", err)
		}
	}
	if err := validateRestrictedSessionPolicy(mode, raw); err != nil {
		return Policy{}, err
	}
	var realRepo string
	var companions []CompanionPolicy
	if mode != agents.ModeBare {
		var err error
		if realRepo, companions, err = validateSessionPolicyRepositories(raw); err != nil {
			return Policy{}, err
		}
	}
	ladder, err := sessionTargetLadder(&raw.Target)
	if err != nil {
		return Policy{}, err
	}
	checkCfg := cfg
	if checkCfg == nil {
		checkCfg = &config.Config{}
	}
	seenTargets := make(map[string]bool, len(ladder))
	for i := range ladder {
		label := sessionTargetLabel(&raw.Target, i)
		agent, ok := agents.Get(ladder[i].Provider)
		if !ok || len(agent.ACP(checkCfg)) == 0 {
			return Policy{}, fmt.Errorf("%s provider has no ACP adapter", label)
		}
		// Every rung must be able to run the mode, not just the one sessions start on: a
		// rotation onto an unqualified provider would hand the model back what the mode took.
		if _, err := agent.ACPRestrictedSessionMeta(mode); err != nil {
			return Policy{}, fmt.Errorf("%s: %w", label, err)
		}
		if cfg != nil {
			account := ladder[i].Account()
			if account == "" {
				account = cfg.DefaultProfileOf(ladder[i].Provider)
			}
			if !box.ProfileAuthed(cfg, ladder[i].Provider, account) {
				return Policy{}, fmt.Errorf("%s credential %q is not authenticated", label, account)
			}
			ladder[i].Accounts = []string{account}
		}
		// Duplicates are checked after credential resolution, so `codex` and `codex@default`
		// are caught as the same rung — a rung that can never be rotated to is a typo.
		if seenTargets[ladder[i].String()] {
			return Policy{}, fmt.Errorf("%s %q is repeated", label, ladder[i].String())
		}
		seenTargets[ladder[i].String()] = true
	}
	if raw.MaxTurns <= 0 || raw.MaxTurns > session.MaxTurnsLimit {
		return Policy{}, fmt.Errorf("max_turns must be between 1 and %d", session.MaxTurnsLimit)
	}
	if raw.MaxQueuedTurns <= 0 || raw.MaxQueuedTurns > session.MaxQueuedTurnsLimit {
		return Policy{}, fmt.Errorf("max_queued_turns must be between 1 and %d", session.MaxQueuedTurnsLimit)
	}
	if raw.MaxQueuedBytes <= 0 || raw.MaxQueuedBytes > session.MaxQueuedBytesLimit {
		return Policy{}, fmt.Errorf("max_queued_bytes must be between 1 and %d", session.MaxQueuedBytesLimit)
	}
	if raw.MaxPatchBytes <= 0 || raw.MaxPatchBytes > sessionWorkspacePatchLimit {
		return Policy{}, fmt.Errorf("max_patch_bytes must be between 1 and %d", sessionWorkspacePatchLimit)
	}
	if raw.TurnTimeout == "" {
		return Policy{}, errors.New("turn_timeout is required")
	}
	timeout, err := time.ParseDuration(raw.TurnTimeout)
	if err != nil || timeout <= 0 || timeout > sessionPolicyMaxTurnTimeout {
		return Policy{}, fmt.Errorf("turn_timeout must be positive and no longer than %s", sessionPolicyMaxTurnTimeout)
	}
	networkPolicy, err := validateSessionEgress(raw.Egress)
	if err != nil {
		return Policy{}, err
	}
	var warmIdleTimeout time.Duration
	if raw.WarmIdleTimeout != "" {
		warmIdleTimeout, err = time.ParseDuration(raw.WarmIdleTimeout)
		if err != nil || warmIdleTimeout <= 0 || warmIdleTimeout > sessionPolicyMaxWarmIdleTimeout {
			return Policy{}, fmt.Errorf("warm_idle_timeout must be positive and no longer than %s", sessionPolicyMaxWarmIdleTimeout)
		}
	}
	return Policy{
		Name: name, Mode: mode, Repository: realRepo, Remote: raw.Remote, Branch: raw.Branch,
		Companions: companions,
		Targets:    ladder,
		// Bare projects nothing a tool-less box could use, and readonly is the read-only bit
		// made a mode: both are implied by the mode, never a second knob to keep in step.
		OmitEnv:            (raw.ProjectEnv != nil && !*raw.ProjectEnv) || mode == agents.ModeBare,
		OmitMCP:            (raw.ProjectMCP != nil && !*raw.ProjectMCP) || mode == agents.ModeBare,
		RepositoryReadOnly: raw.RepositoryReadOnly || mode == agents.ModeReadOnly,
		Egress:             networkPolicy,
		MaxTurns:           raw.MaxTurns,
		MaxQueuedTurns:     raw.MaxQueuedTurns, MaxQueuedBytes: raw.MaxQueuedBytes,
		TurnTimeout: timeout, WarmIdleTimeout: warmIdleTimeout,
		MaxPatchBytes: raw.MaxPatchBytes,
	}, nil
}

// validateSessionPolicyRepositories resolves the primary repository and the companions a policy
// mounts. Every path must be the real root of an existing Git worktree, and no repository may
// appear twice under two names.
func validateSessionPolicyRepositories(raw rawSessionPolicy) (string, []CompanionPolicy, error) {
	if raw.Repository == "" || !filepath.IsAbs(raw.Repository) || filepath.Clean(raw.Repository) != raw.Repository {
		return "", nil, errors.New("repository must be an absolute, clean path")
	}
	realRepo, err := realGitRepository(raw.Repository)
	if err != nil {
		return "", nil, err
	}
	if err := validateSessionRepositorySource(raw.Remote, raw.Branch); err != nil {
		return "", nil, err
	}
	if len(raw.Companions) > sessionPolicyMaxCompanions {
		return "", nil, fmt.Errorf(
			"companions are limited to %d repositories",
			sessionPolicyMaxCompanions,
		)
	}
	companions := make([]CompanionPolicy, 0, len(raw.Companions))
	seenNames := make(map[string]bool, len(raw.Companions))
	seenRepositories := map[string]bool{realRepo: true}
	for _, companion := range raw.Companions {
		if !validCompanionRepositoryName(companion.Name) {
			return "", nil, fmt.Errorf(
				"companion name %q must use 1-48 lowercase letters, numbers, hyphens, or underscores and cannot be primary",
				companion.Name,
			)
		}
		if seenNames[companion.Name] {
			return "", nil, fmt.Errorf("companion name %q is duplicated", companion.Name)
		}
		if companion.Repository == "" || !filepath.IsAbs(companion.Repository) ||
			filepath.Clean(companion.Repository) != companion.Repository {
			return "", nil, fmt.Errorf(
				"companion %q repository must be an absolute, clean path",
				companion.Name,
			)
		}
		realCompanion, err := realGitRepository(companion.Repository)
		if err != nil {
			return "", nil, fmt.Errorf("companion %q: %w", companion.Name, err)
		}
		if err := validateSessionRepositorySource(companion.Remote, companion.Branch); err != nil {
			return "", nil, fmt.Errorf("companion %q: %w", companion.Name, err)
		}
		if seenRepositories[realCompanion] {
			return "", nil, fmt.Errorf(
				"companion %q repeats the primary or another companion repository",
				companion.Name,
			)
		}
		seenNames[companion.Name] = true
		seenRepositories[realCompanion] = true
		companions = append(companions, CompanionPolicy{
			Name: companion.Name, Repository: realCompanion,
			Remote: companion.Remote, Branch: companion.Branch,
		})
	}
	return realRepo, companions, nil
}

// validateRestrictedSessionPolicy refuses, by name, every policy field a restricted mode would
// otherwise silently drop. A bare policy mounts nothing, so any repository-shaped field is a
// contradiction; a readonly policy needs the repository it mounts. Neither keeps a warm box
// (each turn is a fresh box, so an idle lease would keep nothing) nor runs under restricted
// networking, which the restricted profile is not qualified with.
func validateRestrictedSessionPolicy(mode agents.ExecutionMode, raw rawSessionPolicy) error {
	if !mode.Restricted() {
		return nil
	}
	switch mode {
	case agents.ModeBare:
		if raw.Repository != "" || raw.Remote != "" || raw.Branch != "" || len(raw.Companions) > 0 {
			return errors.New("a bare policy names no repository, remote, branch or companion — it mounts none")
		}
		if raw.RepositoryReadOnly {
			return errors.New("a bare policy has no repository to mount read-only — drop repository_read_only")
		}
		if raw.ProjectEnv != nil && *raw.ProjectEnv {
			return errors.New("a bare policy projects no shared environment — drop project_env")
		}
		if raw.ProjectMCP != nil && *raw.ProjectMCP {
			return errors.New("a bare policy mounts no MCP — drop project_mcp")
		}
	case agents.ModeReadOnly:
		if raw.Repository == "" {
			return errors.New("a readonly policy needs the repository it mounts read-only")
		}
	}
	if raw.WarmIdleTimeout != "" {
		return fmt.Errorf("a %s policy is qualified cold only — each turn runs in a fresh box; drop warm_idle_timeout", mode)
	}
	if raw.Egress != nil && raw.Egress.Mode == string(egress.Filtered) {
		return fmt.Errorf("a %s policy is not qualified under restricted networking — set egress.mode to open or none", mode)
	}
	return nil
}

// validateSessionEgress reads the policy's `egress:` block. A missing block is the built-in open
// posture and must stay byte-identical to a file written before this field existed. Rules are
// normalized here, once, so the digest, the admission input and the API all describe the same
// canonical grant — and a posture that cannot enforce a rule refuses the file rather than
// silently accepting rules nothing will apply.
func validateSessionEgress(raw *rawSessionEgressPolicy) (EgressPolicy, error) {
	if raw == nil {
		return EgressPolicy{}, nil
	}
	if raw.Mode == "" {
		return EgressPolicy{}, errors.New("egress.mode is required — open, filtered, or none")
	}
	mode, err := egress.ParseMode(raw.Mode)
	if err != nil {
		return EgressPolicy{}, fmt.Errorf("egress.mode: %w", err)
	}
	if len(raw.Rules) == 0 {
		return EgressPolicy{Mode: mode, ExportDestinations: raw.ExportDestinations}, nil
	}
	if mode != egress.Filtered {
		return EgressPolicy{}, errors.New("egress.rules require egress.mode: filtered")
	}
	rules, err := egress.NormalizeRules(raw.Rules)
	if err != nil {
		return EgressPolicy{}, fmt.Errorf("egress.rules: %w", err)
	}
	return EgressPolicy{Mode: mode, Rules: rules, ExportDestinations: raw.ExportDestinations}, nil
}

// sessionTargetLadder parses a policy's `target:` — one target, or an ordered fallback ladder
// the turn runner rotates through when a rung is rate limited. The shape is a preset's `agent:`
// (preset.leadLadder): a scalar is a single rung, a sequence is the ladder, and the ladder MAY
// be cross-provider. Credential resolution stays with the caller, which holds the config.
func sessionTargetLadder(node *yaml.Node) ([]agents.Target, error) {
	var raw []string
	switch node.Kind {
	case yaml.ScalarNode:
		raw = []string{node.Value}
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			return nil, errors.New("target is an empty list — name at least one target, or write a single one")
		}
		for i, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("target[%d] must be a target (%s), not a map or list", i, sessionTargetGrammar)
			}
			raw = append(raw, item.Value)
		}
	case 0:
		return nil, fmt.Errorf("target is required — a target (%s), or a list of them", sessionTargetGrammar)
	default:
		return nil, fmt.Errorf("target must be a target (%s) or a list of targets, not a map", sessionTargetGrammar)
	}
	if len(raw) > sessionPolicyMaxTargets {
		return nil, fmt.Errorf("target ladder is limited to %d rungs", sessionPolicyMaxTargets)
	}
	ladder := make([]agents.Target, 0, len(raw))
	for i, value := range raw {
		target, err := agents.ParseTarget(value)
		if err != nil {
			return nil, fmt.Errorf("%s %w", sessionTargetLabel(node, i), err)
		}
		// One rung is one credential: a rung IS the concrete thing a turn runs on, and the
		// ladder — not a comma list — is how a policy names an alternative.
		if len(target.Accounts) > 1 {
			return nil, fmt.Errorf("%s must name zero or one credential", sessionTargetLabel(node, i))
		}
		ladder = append(ladder, target)
	}
	return ladder, nil
}

// sessionTargetLabel names the rung a diagnostic is about: bare `target` for a single scalar,
// indexed `target[i]` for a ladder, so the operator can find it in the file.
func sessionTargetLabel(node *yaml.Node, index int) string {
	if node.Kind == yaml.SequenceNode {
		return fmt.Sprintf("target[%d]", index)
	}
	return "target"
}

// sessionTargetList renders a ladder back to the target grammar. A one-rung ladder renders
// exactly as the pre-ladder `target:` string, which keeps existing policy digests stable.
func sessionTargetList(targets []agents.Target) string {
	parts := make([]string, len(targets))
	for i, target := range targets {
		parts[i] = target.String()
	}
	return strings.Join(parts, " ")
}

func validateSessionRepositorySource(remote, branch string) error {
	if remote == "" && branch == "" {
		return nil
	}
	if remote == "" || branch == "" {
		return errors.New("remote and branch must be configured together")
	}
	if len(remote) > 128 || remote[0] == '-' {
		return errors.New("remote must be a safe Git remote name")
	}
	for _, r := range remote {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return errors.New("remote must be a safe Git remote name")
	}
	if len(branch) > 240 || strings.ContainsAny(branch, "\x00\r\n") {
		return errors.New("branch must be a valid Git branch name")
	}
	if err := exec.Command("git", "check-ref-format", "refs/heads/"+branch).Run(); err != nil {
		return errors.New("branch must be a valid Git branch name")
	}
	return nil
}

func validCompanionRepositoryName(name string) bool {
	if name == "" || name == "primary" || len(name) > 48 {
		return false
	}
	for index, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			(index > 0 && (r == '-' || r == '_')) {
			continue
		}
		return false
	}
	return true
}

func realGitRepository(path string) (string, error) {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("repository is not a real path: %w", err)
	}
	if filepath.Clean(realPath) != path {
		return "", errors.New("repository must name its real directory, not a symlink or alias")
	}
	info, err := os.Stat(realPath)
	if err != nil || !info.IsDir() {
		return "", errors.New("repository is not a directory")
	}
	cmd := exec.Command("git", "-C", realPath, "-c", "core.fsmonitor=false", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("repository is not a Git worktree: %w", err)
	}
	root := strings.TrimSpace(string(out))
	root, err = filepath.EvalSymlinks(root)
	if err != nil || filepath.Clean(root) != realPath {
		return "", errors.New("repository path is not the exact Git worktree root")
	}
	return realPath, nil
}

type CreateRemoteSessionRequest struct {
	Policy string `json:"policy"`
	Task   string `json:"task"`
	// Source selects which source INSIDE the policy's already-authorized repository the session
	// starts from. Omitting it is the policy's own default, which is the value host creation
	// supplies when nobody chose another source; a workspace-free session carries none at all.
	// It participates in the request hash, so create, idempotent replay and a fence all bind the
	// same selector.
	Source           *session.SourceSelector   `json:"source,omitempty"`
	ResponderBinding *session.ResponderBinding `json:"responder_binding,omitempty"`
	// ExpectedPolicyDigest / ExpectedAuthorityDigest pin the create to the policy the caller was
	// authorized against (a fleet worker advertises them from its configuration). Admission compares
	// each against the daemon's CURRENT resolution of the named policy and refuses on a mismatch —
	// before any intent is journaled or a workspace exists — so a daemon restarted with a changed
	// same-name policy cannot run a command that was pinned to the old one. Both are optional, so a
	// direct client that pins nothing, and every request hash recorded before the fields existed,
	// are unchanged.
	ExpectedPolicyDigest    string `json:"expected_policy_digest,omitempty"`
	ExpectedAuthorityDigest string `json:"expected_authority_digest,omitempty"`
	// ExpectedNetworkFingerprint pins the create to the network reach the caller was authorized
	// against — the value this daemon published for that policy. Unlike the digests it covers HOST
	// state the policy text cannot express, so an approval edited on this host between the
	// placement and the create refuses instead of running under rules nobody pinned.
	ExpectedNetworkFingerprint string `json:"expected_network_fingerprint,omitempty"`
}

type EnsureWorkspaceTaskRequest struct {
	SessionID        string                    `json:"session_id"`
	ExpectedRevision int64                     `json:"expected_revision"`
	Task             tasks.ControllerTaskDraft `json:"task"`
}

// EnsureWorkspaceTask projects the exact controller-approved task into a writable session before
// its first turn. The filesystem projection is deterministic and the session binding is immutable,
// so a crash between either write and the operation receipt is reconciled by the same request.
func (s *Service) EnsureWorkspaceTask(
	ctx context.Context,
	key string,
	req EnsureWorkspaceTaskRequest,
) (session.Session, error) {
	unlock := s.lockOperation(key)
	defer unlock()
	op, replay, err := s.store.ReserveOperation(ctx, "EnsureWorkspaceTask", key, req)
	if err != nil {
		return session.Session{}, err
	}
	if replay && op.State != session.OperationReserved && op.State != session.OperationRunning {
		return replaySessionOperation(op)
	}
	return s.executeEnsureWorkspaceTask(ctx, op, req)
}

func (s *Service) executeEnsureWorkspaceTask(
	ctx context.Context,
	op session.Operation,
	req EnsureWorkspaceTaskRequest,
) (session.Session, error) {
	if req.SessionID == "" || req.ExpectedRevision <= 0 {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidRequest, Detail: "session and revision are required",
		})
	}
	digest, err := tasks.ControllerTaskDraftSHA256(req.Task)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidRequest, Detail: err.Error(),
		})
	}
	intent, _ := json.Marshal(req)
	if op.State == session.OperationReserved {
		if err := s.store.MarkOperationRunning(ctx, op.ID, intent); err != nil {
			return session.Session{}, err
		}
	}
	sess, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := requireSessionWorkspace(sess); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := validateSessionForkAuthority(ctx, sess); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidSessionState, Detail: err.Error(),
		})
	}
	instance, err := tasks.EnsureControllerTask(sess.Workspace, req.Task)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidSessionState, Detail: err.Error(),
		})
	}
	binding := session.WorkspaceTaskBinding{
		QueueID: instance.Ref.QueueID, TaskID: instance.Ref.TaskID, ID: instance.Ref.ID,
		OfferRef: req.Task.OfferRef, DraftSHA256: digest,
	}
	bound, err := s.store.BindWorkspaceTask(ctx, req.SessionID, req.ExpectedRevision, binding)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	result, err := json.Marshal(bound)
	if err != nil {
		return session.Session{}, err
	}
	if err := s.store.CompleteOperation(ctx, op.ID, "session", bound.ID, result); err != nil {
		return session.Session{}, err
	}
	return bound, nil
}

func cloneResponderBinding(value *session.ResponderBinding) *session.ResponderBinding {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

type Runner interface {
	Run(context.Context, session.Session, session.Turn) (session.Turn, error)
}

type sessionRunnerRuntimeCleaner interface {
	CleanupSession(context.Context, session.Session) error
}

type sessionRunnerParkedCleaner interface {
	CleanupParkedSession(context.Context, session.Session) error
}

type sessionRunnerClosedCleaner interface {
	CleanupClosedSession(context.Context, session.Session) error
}

type sessionRunnerPreparer interface {
	PrepareSession(context.Context, session.Session, time.Duration) error
}

type sessionRunnerWarmInspector interface {
	WarmSessionReady(session.Session) bool
}

type sessionRunnerWarmEvicter interface {
	EvictWarmSession(string) error
}

type sessionRunnerCloser interface {
	CloseWarmSessions() error
}

type sessionRunnerTurnReaper interface {
	ReapInterruptedTurn(context.Context, session.Session, session.Turn) error
}

type RunnerFunc func(context.Context, session.Session, session.Turn) (session.Turn, error)

func (f RunnerFunc) Run(ctx context.Context, sess session.Session, turn session.Turn) (session.Turn, error) {
	return f(ctx, sess, turn)
}

type RunnerFactory func(*session.Store) Runner

type Config struct {
	StateRoot           string
	PolicyPath          string
	Policies            map[string]Policy
	SourceConfig        *config.Config
	Config              *config.Config
	Runtime             runtime.Runtime
	Executable          string
	Host                Host
	Runner              Runner
	RunnerFactory       RunnerFactory
	ReviewGate          ReviewGate
	StopTimeout         time.Duration
	CleanupInterval     time.Duration
	OperationStaleAfter time.Duration
	// StorageLimits overrides the storage policy derived from the volume's measured capacity.
	StorageLimits *StorageLimits
	Logger        *slog.Logger
}

type sessionWorker struct {
	sessionID string
	trigger   chan struct{}
	cancel    context.CancelFunc
	done      chan struct{}
}

type activeSessionTurn struct {
	cancel    context.CancelFunc
	done      chan struct{}
	key       string
	request   session.CancelTurnRequest
	requested bool
}

type pendingSessionCancel struct {
	key     string
	request session.CancelTurnRequest
	ready   chan struct{}
}

type sessionOperationLock struct {
	mu   sync.Mutex
	refs int
}

type runtimeCleanupStamp struct {
	revision        int64
	updatedAt       time.Time
	turnID          string
	candidateSHA256 string
}

type runtimeCleanupCandidate struct {
	session session.Session
	turn    *session.Turn
}

type Service struct {
	store     *session.Store
	stateRoot string
	policies  map[string]Policy
	// policyNetworks is each policy's reach as it resolved when this daemon loaded them: the
	// published fence a placement pins. A create resolves again and compares, so this is what the
	// daemon ADVERTISES, never what it authorizes.
	policyNetworks      map[string]PolicyNetwork
	sourceCfg           *config.Config
	rt                  runtime.Runtime
	executable          string
	host                Host
	runner              Runner
	reviewGate          ReviewGate
	stopTimeout         time.Duration
	cleanupInterval     time.Duration
	operationStaleAfter time.Duration
	log                 *slog.Logger

	stopMu         sync.Mutex
	mu             sync.Mutex
	started        bool
	starting       bool
	ctx            context.Context
	cancel         context.CancelFunc
	workers        map[string]*sessionWorker
	active         map[string]*activeSessionTurn
	pendingCancels map[string]*pendingSessionCancel
	quarantined    map[string]struct{}
	wg             sync.WaitGroup

	operationMu         sync.Mutex
	operationLocks      map[string]*sessionOperationLock
	createActive        map[string]bool
	createSlots         chan struct{}
	testBeforeCreatePin func() error
	testAfterTurnLease  func(session.Turn)
	// testAdmitNetwork replaces create-time network admission. Real admission needs an owner
	// key, an approval and a Docker qualification; a test that only cares what the create path
	// does with the answer injects one. nil in production.
	testAdmitNetwork func(policy Policy, workspace, forkName string) (sessionNetworkBinding, error)
	// testResolveNetwork replaces the create fence's fresh resolution, for the same reason and
	// with the same rule: nil in production.
	testResolveNetwork func(policy Policy) (PolicyNetwork, error)
	// testSessionNetworkReads replaces the evidence read's registry reads: a retained run with
	// denials needs a qualified gateway execution nobody can create in a unit test. nil in production.
	testSessionNetworkReads func(bound session.Session, now time.Time) sessionNetworkReads
	runtimeMu               sync.Mutex
	runtimeLocks            map[string]*sessionOperationLock
	restoring               map[string]bool // sessions whose workspace a restore is rewriting right now
	testDuringRestoreFiles  func()          // test seam: runs while the restore holds the runtime and rewrites files
	runtimeCleanupMu        sync.Mutex
	runtimeCleanupCursor    int
	runtimeCleanupStampMu   sync.Mutex
	runtimeCleanupDone      map[string]runtimeCleanupStamp
	testBeforeCleanupStamp  func()
	historicalMu            sync.Mutex
	historicalPending       map[string]struct{}
	// storage is this worker's own account of the disk it executes on: the configured limits, the
	// sticky allocation decision, and the last measurement. See storage.go.
	storage storageAccountant
}

func NewService(cfg Config) (*Service, error) {
	if cfg.StateRoot == "" {
		return nil, errors.New("session state root is required")
	}
	sourceCfg := cfg.SourceConfig
	if sourceCfg == nil {
		sourceCfg = cfg.Config
	}
	if sourceCfg == nil {
		var err error
		sourceCfg, err = config.Load()
		if err != nil {
			return nil, err
		}
		box.ResolveBaseImage(sourceCfg)
	}
	policies := cfg.Policies
	if len(policies) == 0 {
		var err error
		policies, err = LoadPolicies(cfg.PolicyPath, sourceCfg)
		if err != nil {
			return nil, err
		}
	}
	bound := cloneSessionPolicies(policies)
	// Resolve each policy's network reach BEFORE any state root exists: a policy this host cannot
	// resolve is refused here, with its reason, rather than served with no fence at all.
	networks, err := resolvePolicyNetworks(bound, sourceCfg)
	if err != nil {
		return nil, err
	}
	store, err := session.Open(cfg.StateRoot)
	if err != nil {
		return nil, err
	}
	service := &Service{
		store: store, stateRoot: cfg.StateRoot,
		policies: bound, policyNetworks: networks, sourceCfg: sourceCfg,
		rt: cfg.Runtime, executable: cfg.Executable, host: cfg.Host, runner: cfg.Runner,
		reviewGate:          cfg.ReviewGate,
		stopTimeout:         cfg.StopTimeout,
		cleanupInterval:     cfg.CleanupInterval,
		operationStaleAfter: cfg.OperationStaleAfter,
		log:                 cfg.Logger,
		workers:             make(map[string]*sessionWorker), active: make(map[string]*activeSessionTurn),
		pendingCancels: make(map[string]*pendingSessionCancel),
		quarantined:    make(map[string]struct{}),
		operationLocks: make(map[string]*sessionOperationLock),
		createActive:   make(map[string]bool), createSlots: make(chan struct{}, sessionCreateConcurrency),
		runtimeLocks:       make(map[string]*sessionOperationLock),
		runtimeCleanupDone: make(map[string]runtimeCleanupStamp),
		historicalPending:  make(map[string]struct{}),
	}
	if service.stopTimeout <= 0 {
		service.stopTimeout = DefaultStopTimeout
	}
	if service.cleanupInterval <= 0 {
		service.cleanupInterval = sessionServiceCleanupInterval
	}
	if service.operationStaleAfter <= 0 {
		service.operationStaleAfter = sessionOperationStaleAfter
	}
	if service.log == nil {
		service.log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if cfg.StorageLimits != nil {
		if err := service.setStorageLimits(*cfg.StorageLimits); err != nil {
			return nil, err
		}
	}
	if cfg.RunnerFactory != nil {
		service.runner = cfg.RunnerFactory(store)
	}
	return service, nil
}

func cloneSessionPolicies(in map[string]Policy) map[string]Policy {
	out := make(map[string]Policy, len(in))
	for name, policy := range in {
		if policy.Name == "" {
			policy.Name = name
		}
		policy.Companions = append([]CompanionPolicy(nil), policy.Companions...)
		out[name] = policy
	}
	return out
}

// fenceExpectedPolicyDigests refuses a create whose caller pinned a digest the daemon's current
// resolution of that policy does not produce. The full digest and the model-independent authority
// digest are compared separately, exactly as they are advertised, so a controller may pin only the
// authority shared across conversational/standard/deep policies. It runs at intent capture, so
// the refusal precedes every side effect and a replayed intent is never re-judged.
func fenceExpectedPolicyDigests(policy Policy, req CreateRemoteSessionRequest) error {
	if req.ExpectedPolicyDigest != "" && req.ExpectedPolicyDigest != resolvedSessionPolicyDigest(policy) {
		return &session.Error{Code: session.CodePolicyDigestMismatch,
			Detail: fmt.Sprintf("policy %q no longer resolves to the expected policy digest; the daemon's policy changed since the caller was authorized", policy.Name)}
	}
	if req.ExpectedAuthorityDigest != "" && req.ExpectedAuthorityDigest != ResolvedPolicyAuthorityDigest(policy) {
		return &session.Error{Code: session.CodePolicyDigestMismatch,
			Detail: fmt.Sprintf("policy %q no longer resolves to the expected authority digest; its repository, companions, environment, write mode, or accounts changed since the caller was authorized", policy.Name)}
	}
	return nil
}

// fenceExpectedNetworkFingerprint refuses a create whose caller pinned a network reach this host
// no longer resolves to. It resolves FRESH — the published value is what the daemon advertised at
// startup, and the whole point is to catch an approval edited since then — and it writes nothing
// doing so. Like the digest fence it runs at intent capture, so the refusal precedes every side
// effect: no journaled intent, no workspace, no session row.
func (s *Service) fenceExpectedNetworkFingerprint(policy Policy, req CreateRemoteSessionRequest) error {
	if req.ExpectedNetworkFingerprint == "" {
		return nil // pinned nothing: admission still decides the reach, exactly as before.
	}
	network, err := s.resolvePolicyNetwork(policy)
	if err != nil {
		// A pin nobody can confirm is not a pin. The host's own authority is what failed, so this
		// is the same unavailability a create would meet at admission — reported before the work.
		return &session.Error{Code: session.CodeNetworkUnavailable,
			Detail: fmt.Sprintf("policy %q network cannot be resolved on this host: %v", policy.Name, err)}
	}
	if req.ExpectedNetworkFingerprint == network.Fingerprint {
		return nil
	}
	return &session.Error{Code: session.CodeNetworkFingerprintMismatch,
		Detail: fmt.Sprintf(
			"policy %q now resolves to network %s, not the fingerprint the caller was authorized against; this host's approval changed — run 'coop approve' in %s to see it, then place again against the published fingerprint",
			policy.Name, sessionNetworkReach(network), policy.Repository)}
}

// sessionNetworkReach names the reach in the refusal: an open or offline policy has no
// fingerprint, and saying "" there would read as though the daemon had lost it.
func sessionNetworkReach(network PolicyNetwork) string {
	if network.Fingerprint == "" {
		return string(network.Mode) + " with no captured rules"
	}
	return string(network.Mode) + " " + network.Fingerprint
}

// validSessionDigest is the shape every pinned digest takes: lowercase hex SHA-256.
func validSessionDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func resolvedSessionPolicyDigest(policy Policy) string {
	canonical := struct {
		Name               string            `json:"name"`
		Mode               string            `json:"mode,omitempty"`
		Repository         string            `json:"repository"`
		Remote             string            `json:"remote,omitempty"`
		Branch             string            `json:"branch,omitempty"`
		Companions         []CompanionPolicy `json:"companions,omitempty"`
		Target             string            `json:"target"`
		OmitEnv            bool              `json:"omit_env,omitempty"`
		OmitMCP            bool              `json:"omit_mcp,omitempty"`
		RepositoryReadOnly bool              `json:"repository_read_only,omitempty"`
		Egress             *EgressPolicy     `json:"egress,omitempty"`
		MaxTurns           int               `json:"max_turns"`
		MaxQueuedTurns     int               `json:"max_queued_turns"`
		MaxQueuedBytes     int               `json:"max_queued_bytes"`
		TurnTimeout        int64             `json:"turn_timeout_ns"`
		WarmIdleTimeout    int64             `json:"warm_idle_timeout_ns,omitempty"`
		MaxPatchBytes      int               `json:"max_patch_bytes"`
	}{
		Name: policy.Name, Mode: digestedSessionMode(policy.Mode), Repository: policy.Repository,
		Remote: policy.Remote, Branch: policy.Branch, Companions: policy.Companions,
		Target:  sessionTargetList(policy.Targets),
		OmitEnv: policy.OmitEnv, OmitMCP: policy.OmitMCP,
		RepositoryReadOnly: policy.RepositoryReadOnly,
		Egress:             digestedSessionEgress(policy.Egress),
		MaxTurns:           policy.MaxTurns, MaxQueuedTurns: policy.MaxQueuedTurns,
		MaxQueuedBytes: policy.MaxQueuedBytes, TurnTimeout: int64(policy.TurnTimeout),
		WarmIdleTimeout: int64(policy.WarmIdleTimeout),
		MaxPatchBytes:   policy.MaxPatchBytes,
	}
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// digestedSessionMode contributes the mode to a policy digest only when it is restricted. Normal
// is every policy written before modes existed, and those must keep producing the digest their
// sessions were bound with.
func digestedSessionMode(mode agents.ExecutionMode) string {
	if !mode.Restricted() {
		return ""
	}
	return string(mode)
}

// digestedSessionEgress contributes the network block to a policy digest only when the operator
// wrote one. A file with no `egress:` resolves to the same built-in open posture it always did,
// and must produce the same digest, or every session created before this field existed would stop
// matching its own policy on the next daemon start.
func digestedSessionEgress(policy EgressPolicy) *EgressPolicy {
	if !policy.configured() {
		return nil
	}
	clone := policy
	clone.Rules = append([]egress.Rule(nil), policy.Rules...)
	return &clone
}

// ResolvedPolicyDigest returns the immutable digest bound into a remote session. Operators use
// the same value when authorizing a fleet worker, so the worker cannot advertise a policy name
// whose repository, target, or authority differs from the controller's trusted policy file.
func ResolvedPolicyDigest(policy Policy) string {
	return resolvedSessionPolicyDigest(policy)
}

// ResolvedPolicyAuthorityDigest identifies the authority shared by policies that differ only in
// model, reasoning effort, or resource budgets. Provider accounts remain part of authority: a
// faster model must not silently select a wider credential set.
func ResolvedPolicyAuthorityDigest(policy Policy) string {
	type authorityTarget struct {
		Provider string   `json:"provider"`
		Accounts []string `json:"accounts,omitempty"`
	}
	targets := make([]authorityTarget, 0, len(policy.Targets))
	for _, target := range policy.Targets {
		targets = append(targets, authorityTarget{
			Provider: target.Provider,
			Accounts: append([]string(nil), target.Accounts...),
		})
	}
	canonical := struct {
		Mode               string            `json:"mode,omitempty"`
		Repository         string            `json:"repository"`
		Remote             string            `json:"remote,omitempty"`
		Branch             string            `json:"branch,omitempty"`
		Companions         []CompanionPolicy `json:"companions,omitempty"`
		Targets            []authorityTarget `json:"targets"`
		OmitEnv            bool              `json:"omit_env,omitempty"`
		OmitMCP            bool              `json:"omit_mcp,omitempty"`
		RepositoryReadOnly bool              `json:"repository_read_only,omitempty"`
		Egress             *EgressPolicy     `json:"egress,omitempty"`
	}{
		Mode:       digestedSessionMode(policy.Mode),
		Repository: policy.Repository, Remote: policy.Remote, Branch: policy.Branch,
		Companions: append([]CompanionPolicy(nil), policy.Companions...), Targets: targets,
		OmitEnv: policy.OmitEnv, OmitMCP: policy.OmitMCP,
		RepositoryReadOnly: policy.RepositoryReadOnly,
		// Network reach is authority, not a resource budget: two lanes that differ only in
		// model must not differ in what they can connect to, so this belongs in both digests.
		Egress: digestedSessionEgress(policy.Egress),
	}
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Service) Store() *session.Store { return s.store }

func (s *Service) lockOperation(key string) func() {
	s.operationMu.Lock()
	lock := s.operationLocks[key]
	if lock == nil {
		lock = &sessionOperationLock{}
		s.operationLocks[key] = lock
	}
	lock.refs++
	s.operationMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.operationMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.operationLocks, key)
		}
		s.operationMu.Unlock()
	}
}

// tryLockOperation reserves an idle operation key for watchdog reconciliation
// without waiting behind a live request. Registration and ownership are one
// critical section, so a replay cannot slip between the idle check and claim.
func (s *Service) tryLockOperation(key string) (func(), bool) {
	s.operationMu.Lock()
	if s.operationLocks[key] != nil {
		s.operationMu.Unlock()
		return nil, false
	}
	lock := &sessionOperationLock{refs: 1}
	lock.mu.Lock()
	s.operationLocks[key] = lock
	s.operationMu.Unlock()
	return func() {
		lock.mu.Unlock()
		s.operationMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.operationLocks, key)
		}
		s.operationMu.Unlock()
	}, true
}

func (s *Service) lockSessionRuntime(sessionID string) func() {
	unlock, _ := s.acquireSessionRuntime(sessionID, false)
	return unlock
}

// tryLockSessionRuntime is lockSessionRuntime without the wait: ok is false when a turn, review,
// or cleanup already owns the session's runtime, for callers whose precondition is "parked".
func (s *Service) tryLockSessionRuntime(sessionID string) (func(), bool) {
	return s.acquireSessionRuntime(sessionID, true)
}

// beginWorkspaceRestore takes the session's runtime for a checkpoint restore and marks the
// session as restoring for the duration, so a turn cannot start on the workspace while it is
// being rewritten (the runtime lock) and a turn cannot be queued into that window either
// (SubmitTurn refuses while the mark is set). Exactly one of a restore and a first turn wins:
// a turn queued first makes the restore's "unused session" check refuse before any file work.
func (s *Service) beginWorkspaceRestore(sessionID string) (func(), bool) {
	unlock, ok := s.tryLockSessionRuntime(sessionID)
	if !ok {
		return nil, false
	}
	s.runtimeMu.Lock()
	if s.restoring == nil {
		s.restoring = map[string]bool{}
	}
	s.restoring[sessionID] = true
	s.runtimeMu.Unlock()
	return func() {
		s.runtimeMu.Lock()
		delete(s.restoring, sessionID)
		s.runtimeMu.Unlock()
		unlock()
	}, true
}

func (s *Service) restoreInProgress(sessionID string) bool {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	return s.restoring[sessionID]
}

func (s *Service) acquireSessionRuntime(sessionID string, try bool) (func(), bool) {
	s.runtimeMu.Lock()
	lock := s.runtimeLocks[sessionID]
	if lock == nil {
		lock = &sessionOperationLock{}
		s.runtimeLocks[sessionID] = lock
	}
	lock.refs++
	s.runtimeMu.Unlock()

	release := func() {
		s.runtimeMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.runtimeLocks, sessionID)
		}
		s.runtimeMu.Unlock()
	}
	if try {
		if !lock.mu.TryLock() {
			release()
			return nil, false
		}
	} else {
		lock.mu.Lock()
	}
	return func() {
		lock.mu.Unlock()
		release()
	}, true
}

func (s *Service) Start(parent context.Context) error {
	if parent == nil {
		parent = context.Background()
	}
	s.mu.Lock()
	if s.started || s.starting {
		s.mu.Unlock()
		return nil
	}
	s.starting = true
	s.mu.Unlock()
	if err := s.ensureRunner(); err != nil {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return err
	}
	sessions, err := s.store.ListSessionsForRecovery(parent)
	if err != nil {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return err
	}
	quarantined := make(map[string]struct{})
	var quarantinedIDs []string
	for index := range sessions {
		if sessions[index].State == session.SessionDiscarded {
			continue
		}
		bound, bindErr := s.ensureSessionForkAuthority(parent, sessions[index])
		if bindErr != nil {
			if errors.Is(bindErr, errSessionForkUnproven) {
				s.host.warnf("remote session %s is quarantined: %v; its durable history, workspace, and services were left untouched", sessions[index].ID, bindErr)
				quarantined[sessions[index].ID] = struct{}{}
				quarantinedIDs = append(quarantinedIDs, sessions[index].ID)
				continue
			}
			s.mu.Lock()
			s.starting = false
			s.mu.Unlock()
			return fmt.Errorf("bind remote session %s workspace authority: %w", sessions[index].ID, bindErr)
		}
		sessions[index] = bound
	}
	s.mu.Lock()
	clear(s.quarantined)
	for sessionID := range quarantined {
		s.quarantined[sessionID] = struct{}{}
	}
	s.mu.Unlock()
	cleanupTurns, err := s.store.ListRuntimeCleanupTurns(parent)
	if err != nil {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return err
	}
	reaper, canReap := s.runner.(sessionRunnerTurnReaper)
	byID := make(map[string]session.Session, len(sessions))
	startupAwaitingClean := make(map[string]session.Turn)
	for _, sess := range sessions {
		byID[sess.ID] = sess
	}
	needsReaper := false
	for _, turn := range cleanupTurns {
		if _, skip := quarantined[turn.SessionID]; !skip {
			needsReaper = true
			break
		}
	}
	if needsReaper && !canReap {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return errors.New("startup recovery cannot prove interrupted runtime cleanup")
	}
	var reapErrors []error
	reapFailures := 0
	recordReapError := func(turnID string, err error) {
		reapFailures++
		if len(reapErrors) < startupReapErrorLimit {
			reapErrors = append(reapErrors, fmt.Errorf("turn %s: %s", turnID,
				sessionACPBoundedDetail("runtime cleanup failed", err.Error())))
		}
	}
	for _, turn := range cleanupTurns {
		if _, skip := quarantined[turn.SessionID]; skip {
			continue
		}
		sess, ok := byID[turn.SessionID]
		if !ok {
			recordReapError(turn.ID, fmt.Errorf("session %s is missing", turn.SessionID))
			continue
		}
		if err := requireSessionForkAuthority(sess); err != nil {
			recordReapError(turn.ID, err)
			continue
		}
		if turn.State == session.TurnAwaitingValidation {
			stamp := runtimeCleanupStampFor(sess, &turn)
			if s.runtimeCleanupMatches(sess.ID, stamp) {
				startupAwaitingClean[turn.SessionID] = turn
				continue
			}
		}
		if err := reaper.ReapInterruptedTurn(parent, sess, turn); err != nil {
			recordReapError(turn.ID, err)
			continue
		}
		if turn.State == session.TurnAwaitingValidation {
			startupAwaitingClean[turn.SessionID] = turn
			s.markRuntimeCleanupDone(sess.ID, runtimeCleanupStampFor(sess, &turn))
		}
	}
	if reapFailures > 0 {
		if omitted := reapFailures - len(reapErrors); omitted > 0 {
			reapErrors = append(reapErrors, fmt.Errorf("%d additional runtime cleanup failures", omitted))
		}
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return fmt.Errorf("startup runtime cleanup failed: %w", errors.Join(reapErrors...))
	}
	if _, err := s.store.ReconcileInterruptedTurns(parent, quarantinedIDs...); err != nil {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return err
	}
	sessions, err = s.store.ListSessionsForRecovery(parent)
	if err != nil {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return err
	}
	// Startup does not synchronously scan every historical runtime. Remember
	// the exact pre-existing sessions instead: the janitor handles the idle
	// backlog in bounded batches, while a recovered queued turn cleans its own
	// session just before execution.
	s.historicalMu.Lock()
	for _, sess := range sessions {
		if sess.State != session.SessionDiscarded && requireSessionForkAuthority(sess) == nil {
			if turn, ok := startupAwaitingClean[sess.ID]; ok &&
				sess.Activity == session.ActivityRunning && sess.ActiveTurnID == turn.ID {
				s.markRuntimeCleanupDone(sess.ID, runtimeCleanupStampFor(sess, &turn))
				continue
			}
			s.historicalPending[sess.ID] = struct{}{}
		}
	}
	s.historicalMu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	if s.started {
		s.starting = false
		s.mu.Unlock()
		cancel()
		return nil
	}
	s.started, s.starting, s.ctx, s.cancel = true, false, ctx, cancel
	s.wg.Add(1)
	go s.runSessionMaintenance(ctx)
	s.mu.Unlock()
	// Recover durable cancellation and create intents before re-leasing queued
	// turns. Otherwise a restart can run a turn whose cancellation was already
	// admitted before the crash.
	if err := s.reconcileInterruptedOperations(ctx, true); err != nil {
		_ = s.Stop()
		return fmt.Errorf("reconcile interrupted session operations: %w", err)
	}
	s.mu.Lock()
	for _, sess := range sessions {
		if _, isQuarantined := quarantined[sess.ID]; isQuarantined {
			continue // no worker for a session whose workspace authority is unproven
		}
		if sess.QueuedTurnCount > 0 && requireSessionForkAuthority(sess) == nil {
			s.ensureWorkerLocked(sess.ID)
		}
	}
	for _, worker := range s.workers {
		s.triggerWorker(worker)
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) ensureSessionForkAuthority(ctx context.Context, bound session.Session) (session.Session, error) {
	// Store-level tests and databases created before remote workspaces existed can
	// contain deliberately unbound sessions. They have no fork to reserve; a
	// partially populated binding is still corruption and must fail closed.
	if bound.Repository == "" && bound.Workspace == "" && bound.ForkName == "" && bound.ForkGeneration == "" {
		return bound, nil
	}
	if !validSessionForkBinding(bound) {
		return session.Session{}, errors.New("session workspace binding is invalid")
	}
	unlock, err := forkspace.LockStateContext(ctx, bound.Repository, bound.ForkName)
	if err != nil {
		return session.Session{}, err
	}
	defer unlock()
	identity, ok, err := forkspace.ReadGeneration(bound.Repository, bound.ForkName)
	if err != nil {
		return session.Session{}, err
	}
	if bound.ForkGeneration == "" {
		if !ok {
			return session.Session{}, fmt.Errorf("%w: no generation record exists", errLegacySessionForkUnproven)
		}
		reservation, reserved, reserveErr := forkspace.ReadWorkspaceReservation(bound.Repository, identity)
		if reserveErr != nil {
			return session.Session{}, reserveErr
		}
		if !reserved || reservation.Kind != forkspace.WorkspaceReservationRemoteSession || reservation.OwnerID != bound.ID {
			return session.Session{}, fmt.Errorf("%w: exact session reservation is absent", errLegacySessionForkUnproven)
		}
		if err := forkspace.ValidateGenerationWorkspace(bound.Repository, identity); err != nil {
			return session.Session{}, err
		}
		bound, err = s.store.AdoptSessionForkGeneration(ctx, bound.ID, string(identity.Generation))
		if err != nil {
			return session.Session{}, err
		}
		return bound, nil
	}
	// A missing record, a recreated fork, or a vanished workspace is state that is gone, not a
	// corrupt binding: quarantine this session (its live authority check keeps refusing every
	// operation) rather than refuse to start the daemon for every other session.
	if !ok {
		return session.Session{}, fmt.Errorf("%w: workspace generation record is missing", errSessionForkUnproven)
	}
	if forkspace.Generation(bound.ForkGeneration) != identity.Generation {
		return session.Session{}, fmt.Errorf("%w: workspace generation changed", errSessionForkUnproven)
	}
	if err := forkspace.ValidateGenerationWorkspace(bound.Repository, identity); err != nil {
		return session.Session{}, fmt.Errorf("%w: %v", errSessionForkUnproven, err)
	}
	reservation := forkspace.WorkspaceReservation{
		Version: forkspace.WorkspaceReservationVersion, Fork: identity,
		Kind: forkspace.WorkspaceReservationRemoteSession, OwnerID: bound.ID, CreatedAt: time.Now().UTC(),
	}
	if err := forkspace.ReserveWorkspaceLocked(bound.Repository, reservation); err != nil {
		return session.Session{}, err
	}
	return bound, nil
}

func (s *Service) sessionQuarantined(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, quarantined := s.quarantined[sessionID]
	return quarantined
}

func validSessionForkBinding(bound session.Session) bool {
	return bound.ID != "" && filepath.IsAbs(bound.Repository) && filepath.IsAbs(bound.Workspace) &&
		forkspace.ValidExistingName(bound.ForkName) &&
		bound.Workspace == forkspace.Workspace(bound.Repository, bound.ForkName)
}

// validateSessionForkAuthority is the operation-time half of startup recovery. It never creates
// or adopts authority: a caller about to read or mutate a workspace must prove that the DB binding,
// host generation record, workspace inode, and durable remote-session reservation still agree.
func validateSessionForkAuthority(ctx context.Context, bound session.Session) error {
	return validateSessionForkAuthorityState(ctx, bound, false)
}

func validateSessionForkAuthorityForDiscardPlan(ctx context.Context, bound session.Session) error {
	return validateSessionForkAuthorityState(ctx, bound, true)
}

func validateSessionForkAuthorityState(ctx context.Context, bound session.Session, allowMissingWorkspace bool) error {
	if bound.Repository == "" && bound.Workspace == "" && bound.ForkName == "" && bound.ForkGeneration == "" {
		return nil
	}
	if err := requireSessionForkAuthority(bound); err != nil {
		return err
	}
	if !validSessionForkBinding(bound) {
		return errors.New("session workspace binding is invalid")
	}
	unlock, err := forkspace.LockStateContext(ctx, bound.Repository, bound.ForkName)
	if err != nil {
		return err
	}
	defer unlock()
	identity, ok, err := forkspace.ReadGeneration(bound.Repository, bound.ForkName)
	if err != nil {
		return err
	}
	if !ok || identity.Generation != forkspace.Generation(bound.ForkGeneration) {
		return errors.New("session workspace generation changed")
	}
	if err := forkspace.ValidateGenerationWorkspace(bound.Repository, identity); err != nil &&
		!(allowMissingWorkspace && errors.Is(err, os.ErrNotExist)) {
		return err
	}
	reservation, reserved, err := forkspace.ReadWorkspaceReservation(bound.Repository, identity)
	if err != nil {
		return err
	}
	if !reserved || reservation.Kind != forkspace.WorkspaceReservationRemoteSession || reservation.OwnerID != bound.ID {
		return errors.New("session workspace reservation changed")
	}
	return nil
}

// normalizedSessionMode reads a blank mode as normal: a session replayed from an operation
// receipt written before modes existed carries none, and it ran as every session did then.
func normalizedSessionMode(mode string) string {
	if mode == "" {
		return string(agents.ModeNormal)
	}
	return mode
}

// requireSessionWorkspace refuses a repository-specific operation on a session that has no
// workspace: a bare session by design, or a store-only record nothing ever bound. One sentence,
// one code (a state conflict, not a malformed request), so a client that asked a bare session
// for its changes learns what the session is rather than what its request lacked.
func requireSessionWorkspace(bound session.Session) error {
	if bound.Workspace != "" {
		return nil
	}
	detail := "session has no workspace"
	if bound.Mode == string(agents.ModeBare) {
		detail += ": its policy is bare"
	}
	return &session.Error{Code: session.CodeInvalidSessionState, Detail: detail}
}

// requireSessionForkAuthority is the runtime/destructive-operation fence for a persisted session.
// Fully unbound store-only sessions remain supported, but a bound legacy session cannot touch a
// workspace until an exact host reservation proves which generation it owns.
func requireSessionForkAuthority(bound session.Session) error {
	if bound.Repository == "" && bound.Workspace == "" && bound.ForkName == "" && bound.ForkGeneration == "" {
		return nil
	}
	if bound.ForkGeneration == "" {
		return fmt.Errorf("%w; recreate the session after preserving its workspace", errLegacySessionForkUnproven)
	}
	return nil
}

func (s *Service) runSessionMaintenance(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanupIdleSessionRuntimes(ctx)
			if err := s.reconcileInterruptedOperations(ctx, false); err != nil && ctx.Err() == nil {
				s.log.Error("session operation reconciliation failed", "error", err)
			}
			s.reclaimStorageOnce(ctx)
		}
	}
}

func (s *Service) cleanupIdleSessionRuntimes(ctx context.Context) {
	s.runtimeCleanupMu.Lock()
	defer s.runtimeCleanupMu.Unlock()

	parkedCleaner, parkedOK := s.runner.(sessionRunnerParkedCleaner)
	cleaner, cleanupOK := s.runner.(sessionRunnerRuntimeCleaner)
	reaper, reapOK := s.runner.(sessionRunnerTurnReaper)
	warmInspector, canInspectWarm := s.runner.(sessionRunnerWarmInspector)
	if !parkedOK && !cleanupOK && !reapOK {
		return
	}
	sessions, err := s.store.ListSessionsForRecovery(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.host.warnf("could not list sessions for runtime cleanup: %v", err)
		}
		return
	}
	awaiting := make(map[string]session.Turn)
	if reapOK {
		turns, listErr := s.store.ListRuntimeCleanupTurns(ctx)
		if listErr != nil {
			if ctx.Err() == nil {
				s.host.warnf("could not list turns for runtime cleanup: %v", listErr)
			}
			return
		}
		for _, turn := range turns {
			if turn.State == session.TurnAwaitingValidation {
				awaiting[turn.SessionID] = turn
			}
		}
	}
	candidates := make([]runtimeCleanupCandidate, 0, len(sessions))
	eligible := make(map[string]struct{})
	for _, candidate := range sessions {
		if candidate.State == session.SessionDiscarded {
			continue
		}
		if err := validateSessionForkAuthority(ctx, candidate); err != nil {
			continue
		}
		var turn *session.Turn
		if awaitingTurn, ok := awaiting[candidate.ID]; ok &&
			candidate.Activity == session.ActivityRunning && candidate.ActiveTurnID == awaitingTurn.ID {
			copy := awaitingTurn
			turn = &copy
		} else if !parkedOK && !cleanupOK {
			continue
		} else if candidate.Activity != session.ActivityParked || candidate.ActiveTurnID != "" {
			continue
		}
		candidates = append(candidates, runtimeCleanupCandidate{session: candidate, turn: turn})
		eligible[candidate.ID] = struct{}{}
	}
	s.pruneRuntimeCleanupDone(eligible)
	if len(candidates) == 0 {
		s.runtimeCleanupCursor = 0
		return
	}
	start := s.runtimeCleanupCursor % len(candidates)
	scanned, attempts := 0, 0
	for scanned < len(candidates) && attempts < runtimeCleanupBatchSize {
		candidate := candidates[(start+scanned)%len(candidates)]
		scanned++
		stamp := runtimeCleanupStampFor(candidate.session, candidate.turn)
		if s.runtimeCleanupMatches(candidate.session.ID, stamp) {
			continue
		}
		attempts++
		unlock := s.lockSessionRuntime(candidate.session.ID)
		current, getErr := s.store.GetSession(ctx, candidate.session.ID)
		currentTurn := session.Turn{}
		cleaned, warmReady := false, false
		if getErr == nil && candidate.turn != nil {
			currentTurn, getErr = s.store.GetTurn(ctx, current.ID, candidate.turn.ID)
			if getErr == nil && current.Activity == session.ActivityRunning &&
				current.ActiveTurnID == currentTurn.ID && currentTurn.State == session.TurnAwaitingValidation &&
				currentTurn.Candidate != nil && currentTurn.CandidateSHA256 == candidate.turn.CandidateSHA256 {
				getErr = reaper.ReapInterruptedTurn(ctx, current, currentTurn)
				cleaned = getErr == nil
			}
		} else if getErr == nil && current.Activity == session.ActivityParked && current.ActiveTurnID == "" {
			warmReady = canInspectWarm && warmInspector.WarmSessionReady(current)
			ranCleanup := false
			if parkedOK {
				ranCleanup = true
				getErr = parkedCleaner.CleanupParkedSession(ctx, current)
			} else if cleanupOK {
				ranCleanup = true
				getErr = cleaner.CleanupSession(ctx, current)
			}
			cleaned = ranCleanup && getErr == nil && !warmReady
		}
		if cleaned {
			if s.testBeforeCleanupStamp != nil {
				s.testBeforeCleanupStamp()
			}
			var turn *session.Turn
			if currentTurn.ID != "" {
				turn = &currentTurn
			}
			s.markRuntimeCleanupDone(current.ID, runtimeCleanupStampFor(current, turn))
			s.markHistoricalRuntimeClean(current.ID)
		}
		unlock()
		if getErr != nil && ctx.Err() == nil {
			s.host.warnf("could not clean session runtime state %s: %v", candidate.session.ID, getErr)
		}
	}
	s.runtimeCleanupCursor = (start + scanned) % len(candidates)
}

func runtimeCleanupStampFor(sess session.Session, turn *session.Turn) runtimeCleanupStamp {
	stamp := runtimeCleanupStamp{revision: sess.Revision, updatedAt: sess.UpdatedAt}
	if turn != nil {
		stamp.turnID = turn.ID
		stamp.candidateSHA256 = turn.CandidateSHA256
	}
	return stamp
}

func (s *Service) runtimeCleanupMatches(sessionID string, stamp runtimeCleanupStamp) bool {
	s.runtimeCleanupStampMu.Lock()
	defer s.runtimeCleanupStampMu.Unlock()
	cleaned, ok := s.runtimeCleanupDone[sessionID]
	return ok && cleaned == stamp
}

func (s *Service) markRuntimeCleanupDone(sessionID string, stamp runtimeCleanupStamp) {
	s.runtimeCleanupStampMu.Lock()
	s.runtimeCleanupDone[sessionID] = stamp
	s.runtimeCleanupStampMu.Unlock()
}

func (s *Service) invalidateRuntimeCleanup(sessionID string) {
	s.runtimeCleanupStampMu.Lock()
	delete(s.runtimeCleanupDone, sessionID)
	s.runtimeCleanupStampMu.Unlock()
}

func (s *Service) pruneRuntimeCleanupDone(eligible map[string]struct{}) {
	s.runtimeCleanupStampMu.Lock()
	defer s.runtimeCleanupStampMu.Unlock()
	for sessionID := range s.runtimeCleanupDone {
		if _, ok := eligible[sessionID]; !ok {
			delete(s.runtimeCleanupDone, sessionID)
		}
	}
}

func (s *Service) runBoundSessionTurn(ctx context.Context, bound session.Session, leased session.Turn) (session.Turn, error) {
	if err := validateSessionForkAuthority(ctx, bound); err != nil {
		return leased, err
	}
	unlock := s.lockSessionRuntime(bound.ID)
	defer unlock()
	if s.historicalRuntimeNeedsCleanup(bound.ID) {
		if cleaner, ok := s.runner.(sessionRunnerRuntimeCleaner); ok {
			if err := cleaner.CleanupSession(ctx, bound); err != nil {
				return leased, fmt.Errorf("clean historical session runtime: %w", err)
			}
			s.markHistoricalRuntimeClean(bound.ID)
		}
	}
	return s.runner.Run(s.sessionTurnContext(ctx, bound), bound, leased)
}

func (s *Service) historicalRuntimeNeedsCleanup(sessionID string) bool {
	s.historicalMu.Lock()
	defer s.historicalMu.Unlock()
	_, pending := s.historicalPending[sessionID]
	return pending
}

func (s *Service) markHistoricalRuntimeClean(sessionID string) {
	s.historicalMu.Lock()
	delete(s.historicalPending, sessionID)
	s.historicalMu.Unlock()
}

// sessionTurnContext decorates a turn's context with what the operator policy still authorizes
// for this session: the warm idle lease and the rotation ladder.
//
// The warm lease stays digest-strict — keeping an authenticated process alive is a standing
// grant, and a drifted policy must not extend it. The ladder deliberately is NOT: it applies
// whenever the session's current target is one of the CURRENT policy's rungs. Rotation can only
// ever move a session between rungs the operator has just named, and only if the session already
// sits on one, so no stale policy steers anything. Requiring the digest instead left every
// session that survived a policy edit without a fallback — pinned to a rate-limited rung,
// failing the exact turns the ladder in the file existed to save. A session on a rung the
// operator has since REMOVED keeps its pinned target and does not rotate.
func (s *Service) sessionTurnContext(ctx context.Context, bound session.Session) context.Context {
	policy, ok := s.policies[bound.Policy]
	if !ok {
		return ctx
	}
	if policy.WarmIdleTimeout > 0 && resolvedSessionPolicyDigest(policy) == bound.PolicyDigest {
		ctx = context.WithValue(ctx, sessionWarmIdleTimeoutContextKey{}, policy.WarmIdleTimeout)
	}
	if len(policy.Targets) > 1 {
		for _, rung := range policy.Targets {
			if rung.String() == bound.Target {
				return context.WithValue(ctx, sessionTargetLadderContextKey{}, policy.Targets)
			}
		}
	}
	return ctx
}

// ensureRunner keeps host-local construction (and runtime detection) out of service creation.
// Opening the service first lets callers acquire the durable state-root lock before doing any
// runner-specific startup work, and lets pure local commands remain usable without a runtime.
func (s *Service) ensureRunner() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runner != nil {
		s.defaultReviewGateLocked()
		return nil
	}
	if s.rt.Name == "" {
		rt, err := runtime.Detect(s.sourceCfg.RuntimeName)
		if err != nil {
			return err
		}
		s.rt = rt
	}
	runner := newSessionTurnRunner(s.sourceCfg, s.store.Root(), s.store, s.rt, s.executable)
	runner.host = s.host
	s.runner = runner
	s.defaultReviewGateLocked()
	return nil
}

// defaultReviewGateLocked fills in the host's gate for a caller that injected none, and only then
// — the detected runtime is an input, so the gate cannot be built before this point.
func (s *Service) defaultReviewGateLocked() {
	if s.reviewGate == nil && s.host.ReviewGateFactory != nil {
		s.reviewGate = s.host.ReviewGateFactory(s.sourceCfg, s.rt)
	}
}

func (s *Service) Stop() error {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()

	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		if closer, ok := s.runner.(sessionRunnerCloser); ok {
			if err := closer.CloseWarmSessions(); err != nil {
				return err
			}
		}
		return s.store.Close()
	}
	cancel := s.cancel
	s.started = false
	workers := make([]*sessionWorker, 0, len(s.workers))
	for _, worker := range s.workers {
		workers = append(workers, worker)
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, worker := range workers {
		worker.cancel()
	}
	// Every session-create lock and Git subprocess is context-aware. Waiting
	// here is therefore the proof that no old worker can mutate after the store
	// closes or a replacement service starts.
	s.wg.Wait()
	s.mu.Lock()
	s.workers = make(map[string]*sessionWorker)
	s.active = make(map[string]*activeSessionTurn)
	s.pendingCancels = make(map[string]*pendingSessionCancel)
	s.mu.Unlock()
	if closer, ok := s.runner.(sessionRunnerCloser); ok {
		if err := closer.CloseWarmSessions(); err != nil {
			return err
		}
	}
	if err := s.store.Close(); err != nil {
		return err
	}
	return nil
}

func (s *Service) ensureWorkerLocked(sessionID string) *sessionWorker {
	if worker := s.workers[sessionID]; worker != nil {
		return worker
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &sessionWorker{sessionID: sessionID, trigger: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{})}
	s.workers[sessionID] = worker
	s.wg.Add(1)
	go s.runSessionWorker(workerCtx, worker)
	return worker
}

func (s *Service) triggerWorker(worker *sessionWorker) {
	if worker == nil {
		return
	}
	select {
	case worker.trigger <- struct{}{}:
	default:
	}
}

func (s *Service) schedule(sessionID string) {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	worker := s.ensureWorkerLocked(sessionID)
	s.triggerWorker(worker)
	s.mu.Unlock()
}

func (s *Service) runSessionWorker(ctx context.Context, worker *sessionWorker) {
	defer s.wg.Done()
	defer s.removeWorker(worker)
	defer close(worker.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-worker.trigger:
			for {
				s.drainSession(ctx, worker.sessionID)
				s.mu.Lock()
				if s.workers[worker.sessionID] != worker {
					s.mu.Unlock()
					return
				}
				select {
				case <-worker.trigger:
					s.mu.Unlock()
					continue
				default:
					delete(s.workers, worker.sessionID)
					s.mu.Unlock()
					return
				}
			}
		}
	}
}

func (s *Service) removeWorker(worker *sessionWorker) {
	if worker == nil {
		return
	}
	s.mu.Lock()
	if s.workers[worker.sessionID] == worker {
		delete(s.workers, worker.sessionID)
	}
	s.mu.Unlock()
}

func (s *Service) drainSession(ctx context.Context, sessionID string) {
	for ctx.Err() == nil {
		bound, err := s.store.GetSession(ctx, sessionID)
		if err != nil {
			return
		}
		if bound.State == session.SessionClosed || bound.State == session.SessionDiscarded {
			return
		}
		if err := validateSessionForkAuthority(ctx, bound); err != nil {
			return
		}
		leased, ok, err := s.store.LeaseNextTurn(ctx, sessionID)
		if err != nil || !ok {
			return
		}
		if s.testAfterTurnLease != nil {
			s.testAfterTurnLease(leased)
		}
		turnCtx, turnCancel := context.WithTimeout(ctx, bound.TurnTimeout)
		active := &activeSessionTurn{cancel: turnCancel, done: make(chan struct{})}
		s.mu.Lock()
		pending := s.pendingCancels[leased.ID]
		if pending != nil {
			active.key, active.request, active.requested = pending.key, pending.request, true
			delete(s.pendingCancels, leased.ID)
		}
		s.active[leased.ID] = active
		s.mu.Unlock()
		if pending != nil {
			close(pending.ready)
			turnCancel()
		}
		runCtx := context.WithValue(turnCtx, sessionCancelRequestContextKey{}, func() (string, session.CancelTurnRequest, bool) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if !active.requested {
				return "", session.CancelTurnRequest{}, false
			}
			return active.key, active.request, true
		})
		_, runErr := s.runBoundSessionTurn(runCtx, bound, leased)
		turnCancel()
		s.mu.Lock()
		requested, cancelKey, cancelReq := active.requested, active.key, active.request
		delete(s.active, leased.ID)
		s.mu.Unlock()
		if requested && !sessionRunnerCleanupFailed(runErr) {
			// The real ACP runner performs this after child cleanup. This fallback makes injected
			// runners obey the same durable rule without making tests depend on ACP internals.
			cancelled, cancelErr := s.store.CancelTurn(context.Background(), cancelKey, cancelReq)
			if cancelErr == nil {
				_ = cancelled
			} else if session.CodeOf(cancelErr) != session.CodeTurnNotRunnable {
				runErr = errors.Join(runErr, cancelErr)
			}
		}
		close(active.done)
		if runErr != nil {
			// A test runner may return an error without terminalizing its lease. Do not leave a
			// durable starting turn wedged; the production runner already records its own detail.
			current, getErr := s.store.GetTurn(context.Background(), sessionID, leased.ID)
			if getErr == nil && (current.State == session.TurnStarting || current.State == session.TurnRunning) && !requested {
				_, _ = s.store.FailTurn(context.Background(), session.FailTurnRequest{SessionID: sessionID, TurnID: leased.ID, ErrorCode: session.CodeInternal, ErrorDetail: boundedSessionServiceError(runErr)})
			}
			code := session.CodeOf(runErr)
			if code == "" {
				code = session.CodeInternal
			}
			s.log.Error("session turn failed",
				"session_id", sessionID, "turn_id", leased.ID,
				"error_code", code, "error_detail", s.operationalErrorDetail(runErr),
			)
		}
	}
}

func sessionRunnerCleanupFailed(err error) bool {
	var failure *sessionACPFailure
	return errors.As(err, &failure) && failure.code == sessionACPCleanupError
}

func boundedSessionServiceError(err error) string {
	if err == nil {
		return "session turn failed"
	}
	detail := err.Error()
	detail = strings.ToValidUTF8(detail, "�")
	if len(detail) > session.MaxErrorDetailBytes {
		detail = detail[:session.MaxErrorDetailBytes]
	}
	return detail
}

// PolicyNetworks is what this daemon advertises for the policies it serves: each policy's mode and,
// for a filtered one, the fingerprint a create may pin. Each answer is resolved NOW, not when the
// daemon started, so approving a change on this host changes what callers are told without a
// restart — otherwise a caller would keep pinning a value every create then refuses. Resolving
// reads host state and publishes nothing.
//
// A policy that cannot be resolved at this moment is reported by mode alone: the fingerprint is
// what a create would pin, and right now there is none. The create itself still reports why.
func (s *Service) PolicyNetworks() map[string]PolicyNetwork {
	if len(s.policyNetworks) == 0 {
		return nil
	}
	networks := make(map[string]PolicyNetwork, len(s.policyNetworks))
	for name, loaded := range s.policyNetworks {
		policy, err := s.policy(name)
		if err != nil {
			networks[name] = loaded
			continue
		}
		current, err := s.resolvePolicyNetwork(policy)
		if err != nil {
			networks[name] = PolicyNetwork{Mode: loaded.Mode}
			continue
		}
		networks[name] = current
	}
	return networks
}

func (s *Service) policy(name string) (Policy, error) {
	if name == "" {
		return Policy{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "policy name is required"}
	}
	policy, ok := s.policies[name]
	if !ok {
		return Policy{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "unknown execution policy"}
	}
	return policy, nil
}

func (s *Service) CreateRemoteSession(ctx context.Context, key string, req CreateRemoteSessionRequest) (session.Session, error) {
	op, err := s.beginCreateOperation(ctx, key, req)
	if err != nil {
		return session.Session{}, err
	}
	if s.serviceRunning() {
		if op.State == session.OperationRunning {
			s.scheduleCreateOperation(op.ID)
		}
		return s.waitForCreateOperation(ctx, op)
	}
	return s.replayCreateOperation(ctx, op)
}

func (s *Service) serviceRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started && s.ctx != nil
}

func (s *Service) waitForCreateOperation(
	ctx context.Context,
	op session.Operation,
) (session.Session, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for op.State == session.OperationReserved || op.State == session.OperationRunning {
		select {
		case <-ctx.Done():
			return session.Session{}, wrapServiceOperationError(op.ID, ctx.Err())
		case <-ticker.C:
		}
		var err error
		op, err = s.store.GetOperationByID(ctx, op.ID)
		if err != nil {
			return session.Session{}, wrapServiceOperationError(op.ID, err)
		}
	}
	return s.replayCreateOperation(ctx, op)
}

// CreateRemoteSessionAsync durably admits a create before returning. Slow Git
// resolution and workspace materialization run under the service lifetime,
// independent of the HTTP request that admitted them.
func (s *Service) CreateRemoteSessionAsync(
	ctx context.Context,
	key string,
	req CreateRemoteSessionRequest,
) (session.Operation, error) {
	op, err := s.beginCreateOperation(ctx, key, req)
	if err != nil {
		return session.Operation{}, err
	}
	if op.State == session.OperationRunning {
		s.scheduleCreateOperation(op.ID)
	}
	return op, nil
}

func (s *Service) beginCreateOperation(
	ctx context.Context,
	key string,
	req CreateRemoteSessionRequest,
) (session.Operation, error) {
	if req.Policy == "" || len(req.Policy) > session.MaxIDBytes || !utf8SessionText(req.Policy) || req.Task == "" || len(req.Task) > session.MaxExternalRefBytes || !utf8SessionText(req.Task) {
		return session.Operation{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "policy and bounded task are required"}
	}
	if req.Source != nil {
		if err := session.ValidateSourceSelectorShape(*req.Source); err != nil {
			return session.Operation{}, err
		}
	}
	if err := session.ValidateResponderBinding(req.ResponderBinding); err != nil {
		return session.Operation{}, err
	}
	for _, expected := range []string{req.ExpectedPolicyDigest, req.ExpectedAuthorityDigest, req.ExpectedNetworkFingerprint} {
		if expected != "" && !validSessionDigest(expected) {
			return session.Operation{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "expected policy digests must be lowercase hex SHA-256"}
		}
	}
	op, replay, err := s.store.ReserveOperation(ctx, "CreateRemoteSession", key, req)
	if err != nil {
		return session.Operation{}, err
	}
	if replay {
		if op.State != session.OperationReserved {
			return op, nil
		}
	}
	intent, err := s.captureCreateIntent(op, req)
	if err != nil {
		return session.Operation{}, s.failServiceOperation(ctx, op.ID, err)
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return session.Operation{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := s.store.MarkOperationRunning(ctx, op.ID, data); err != nil {
		if replay {
			latest, getErr := s.store.GetOperationByID(ctx, op.ID)
			if getErr == nil && latest.State != session.OperationReserved {
				return latest, nil
			}
		}
		return session.Operation{}, err
	}
	return s.store.GetOperationByID(ctx, op.ID)
}

func (s *Service) replayCreateOperation(ctx context.Context, op session.Operation) (session.Session, error) {
	switch op.State {
	case session.OperationSucceeded:
		var sess session.Session
		if err := json.Unmarshal(op.Result, &sess); err != nil {
			return session.Session{}, wrapServiceOperationError(op.ID,
				fmt.Errorf("decode create operation result: %w", err))
		}
		if sess.ID == "" {
			return session.Session{}, wrapServiceOperationError(op.ID,
				errors.New("decode create operation result: missing session id"))
		}
		return sess, nil
	case session.OperationFailed:
		return session.Session{}, &serviceOperationError{
			operationID: op.ID,
			err:         &session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail},
		}
	case session.OperationRunning:
		var intent sessionCreateIntent
		if err := json.Unmarshal(op.Result, &intent); err != nil {
			return s.rejectCreateIntent(ctx, op.ID, "create operation intent is unreadable")
		}
		return s.executeCreateIntent(ctx, op, intent)
	default:
		return session.Session{}, &serviceOperationError{
			operationID: op.ID,
			err:         session.ErrOperationUncertain,
		}
	}
}

type sessionCreateIntent struct {
	OperationID     string `json:"operation_id"`
	Policy          Policy `json:"policy"`
	Task            string `json:"task"`
	SessionID       string `json:"session_id"`
	ForkName        string `json:"fork_name"`
	BaseCommit      string `json:"base_commit"`
	WorkspaceCommit string `json:"workspace_commit"`
	// Source is the normalized request captured at admission, before any remote is contacted;
	// SourceBinding is the immutable identity the pin phase resolved from it and replaced this
	// intent with, so a replay reuses exact objects instead of landing on a branch that moved.
	Source        *session.SourceSelector `json:"source,omitempty"`
	SourceBinding *session.SourceBinding  `json:"source_binding,omitempty"`
	// RetiredPullRequest detects a create intent journaled by a binary that only knew the
	// pull-request dialect. It is never written and never read as a source: replaying such an
	// intent as "default" would quietly move an approved pull-request session onto the default
	// branch, so its presence fails the replay closed instead.
	RetiredPullRequest  json.RawMessage                      `json:"pull_request,omitempty"`
	ResponderBinding    *session.ResponderBinding            `json:"responder_binding,omitempty"`
	Companions          []session.CompanionRepository        `json:"companions,omitempty"`
	RepositoryFreshness []session.RepositoryFreshnessReceipt `json:"repository_freshness,omitempty"`
}

func (s *Service) captureCreateIntent(op session.Operation, req CreateRemoteSessionRequest) (sessionCreateIntent, error) {
	policy, err := s.policy(req.Policy)
	if err != nil {
		return sessionCreateIntent{}, err
	}
	if err := fenceExpectedPolicyDigests(policy, req); err != nil {
		return sessionCreateIntent{}, err
	}
	if err := s.fenceExpectedNetworkFingerprint(policy, req); err != nil {
		return sessionCreateIntent{}, err
	}
	if policy.Mode == agents.ModeBare {
		if req.Source != nil {
			return sessionCreateIntent{}, &session.Error{Code: session.CodeInvalidRequest,
				Detail: "a bare session has no repository to select a source in"}
		}
		if req.ResponderBinding != nil {
			return sessionCreateIntent{}, &session.Error{Code: session.CodeInvalidRequest,
				Detail: "a bare session binds no Responder MCP endpoint — it runs no tool"}
		}
	}
	// An offline session's box reaches no server by URL, so its MCP copy carries only local
	// servers — the Responder endpoint among the ones left out. Refuse the binding here rather
	// than accept authority the turn would silently drop.
	if policy.Egress.resolvedMode() == egress.None && req.ResponderBinding != nil {
		return sessionCreateIntent{}, &session.Error{Code: session.CodeInvalidRequest,
			Detail: responderNeedsNetwork}
	}
	sessionID := deterministicSessionID(op.ID)
	companions := make([]session.CompanionRepository, 0, len(policy.Companions))
	for _, companion := range policy.Companions {
		workspace, err := sessionCompanionWorkspace(
			s.store.Root(), sessionID, companion.Name,
		)
		if err != nil {
			return sessionCreateIntent{}, err
		}
		companions = append(companions, session.CompanionRepository{
			Name: companion.Name, Repository: companion.Repository,
			Workspace: workspace,
		})
	}
	intent := sessionCreateIntent{
		OperationID: op.ID, Policy: policy, Task: req.Task,
		SessionID: sessionID, ForkName: deterministicForkName(op.ID), Companions: companions,
		ResponderBinding: cloneResponderBinding(req.ResponderBinding),
	}
	if policy.Mode != agents.ModeBare {
		// Every repository-backed session records the exact selector it was admitted with,
		// including the default one the host supplies when nobody chose another source.
		normalized := session.DefaultSourceSelector()
		if req.Source != nil {
			normalized = *req.Source
		}
		if err := validateSessionSourceSelector(policy, normalized); err != nil {
			return sessionCreateIntent{}, err
		}
		intent.Source = &normalized
	}
	return intent, nil
}

func (s *Service) runCreateOperation(ctx context.Context, operationID string) error {
	op, err := s.store.GetOperationByID(ctx, operationID)
	if err != nil || op.State != session.OperationRunning || op.Method != "CreateRemoteSession" {
		return err
	}
	unlock := s.lockOperation(op.IdempotencyKey)
	defer unlock()
	op, err = s.store.GetOperationByID(ctx, operationID)
	if err != nil || op.State != session.OperationRunning {
		return err
	}
	_, err = s.replayCreateOperation(ctx, op)
	return err
}

func deterministicSessionID(operationID string) string {
	sum := sha256.Sum256([]byte(operationID))
	return "remote_" + hex.EncodeToString(sum[:16])
}

func deterministicForkName(operationID string) string {
	sum := sha256.Sum256([]byte("fork\x00" + operationID))
	return "remote-" + hex.EncodeToString(sum[:12])
}

func (s *Service) executeCreateIntent(ctx context.Context, op session.Operation, intent sessionCreateIntent) (session.Session, error) {
	if intent.OperationID != op.ID || intent.SessionID != deterministicSessionID(op.ID) ||
		intent.ForkName != deterministicForkName(op.ID) {
		return s.rejectCreateIntent(ctx, op.ID, "create operation intent is invalid")
	}
	if len(intent.RetiredPullRequest) != 0 {
		return s.rejectCreateIntent(ctx, op.ID,
			"create operation intent selects a source through the retired pull-request dialect; request the session again with a source selector")
	}
	if intent.Policy.Mode != agents.ModeBare && intent.Source == nil {
		// A repository-backed intent journaled before source selection existed asked for exactly
		// the policy's own configured branch, which is what the default selector resolves to — so
		// normalizing it replays the SAME request rather than choosing a new one. An intent that
		// selected a pull request never reaches here; it is refused above, because replaying it
		// as the default would quietly move an approved session onto the default branch.
		normalized := session.DefaultSourceSelector()
		intent.Source = &normalized
	}
	sessionExisted := false
	if existing, err := s.store.GetSession(ctx, intent.SessionID); err == nil {
		sessionExisted = true
		if err := requireSessionForkAuthority(existing); errors.Is(err, errLegacySessionForkUnproven) {
			return session.Session{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
		}
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return session.Session{}, err
	}
	if !validSessionIntentTargets(intent.Policy.Targets) {
		return s.rejectCreateIntent(ctx, op.ID, "create operation intent has no valid target")
	}
	expectedIntent := op.Result
	createReq := session.CreateSessionRequest{
		// A session starts on the ladder's first rung; a rate limit rotates it to the next.
		ID: intent.SessionID, ExternalRef: intent.Task, Target: intent.Policy.Targets[0].String(), Policy: intent.Policy.Name,
		PolicyDigest:       resolvedSessionPolicyDigest(intent.Policy),
		AuthorityDigest:    ResolvedPolicyAuthorityDigest(intent.Policy),
		Mode:               string(intent.Policy.Mode),
		OmitEnv:            intent.Policy.OmitEnv,
		OmitMCP:            intent.Policy.OmitMCP,
		ResponderBinding:   cloneResponderBinding(intent.ResponderBinding),
		RepositoryReadOnly: intent.Policy.RepositoryReadOnly,
		MaxTurns:           intent.Policy.MaxTurns,
		MaxQueuedTurns:     intent.Policy.MaxQueuedTurns, MaxQueuedBytes: intent.Policy.MaxQueuedBytes,
		TurnTimeout: intent.Policy.TurnTimeout, MaxPatchBytes: intent.Policy.MaxPatchBytes,
	}
	var workspace sessionWorkspace
	var companions []session.CompanionRepository
	failCreate := func(cause error) (session.Session, error) {
		if ctx.Err() != nil && errors.Is(cause, ctx.Err()) {
			// Deterministic intent and workspace names make restart recovery the
			// safe cleanup owner. Contextless rollback here would outlive Stop and
			// race that recovery process.
			return session.Session{}, cause
		}
		if workspace.Path != "" {
			if cleanupErr := rollbackSessionCreate(workspace, companions); cleanupErr != nil {
				cause = errors.Join(
					cause,
					fmt.Errorf("rollback partial session creation: %w", cleanupErr),
				)
			}
		}
		return session.Session{}, s.failServiceOperation(ctx, op.ID, cause)
	}
	if intent.Policy.Mode == agents.ModeBare {
		// A bare session has nothing to pin, fork, clone or admit: no Git runs, no workspace
		// exists, and its network posture is the policy's own (open unless it wrote none) —
		// there is no project whose remembered approval could widen or narrow it.
		if intent.BaseCommit != "" || intent.WorkspaceCommit != "" || len(intent.RepositoryFreshness) != 0 ||
			len(intent.Companions) != 0 || intent.Source != nil || intent.Policy.Repository != "" {
			return s.rejectCreateIntent(ctx, op.ID, "create operation intent names a repository for a bare policy")
		}
		createReq.NetworkMode = string(intent.Policy.Egress.resolvedMode())
	} else {
		if intent.BaseCommit == "" {
			if intent.WorkspaceCommit != "" {
				return s.rejectCreateIntent(ctx, op.ID, "create operation intent has incomplete repository pins")
			}
			var err error
			intent, err = s.pinCreateIntent(ctx, op, intent)
			if err != nil {
				if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
					return session.Session{}, err
				}
				return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
			}
			expectedIntent, err = json.Marshal(intent)
			if err != nil {
				return session.Session{}, err
			}
		} else if len(intent.RepositoryFreshness) == 0 {
			return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
				Code:   session.CodeRepositoryUnavailable,
				Detail: "repository freshness must be reacquired by a new session request",
			})
		}
		if intent.Policy.Remote != "" && intent.SourceBinding == nil {
			// A remote-backed policy always resolves a binding; an intent pinned without one
			// predates source selection and must be requested again rather than materialized
			// into a session no controller could validate.
			return s.rejectCreateIntent(ctx, op.ID, "create operation intent has no resolved source binding")
		}
		workspaceCommit := intent.WorkspaceCommit
		if workspaceCommit == "" {
			// Compatibility with create intents reserved before workspace_commit was added.
			workspaceCommit = intent.BaseCommit
		}
		if !validSessionWorkspaceCommit(intent.BaseCommit) ||
			!validSessionWorkspaceCommit(workspaceCommit) {
			return s.rejectCreateIntent(ctx, op.ID, "create operation intent has invalid repository pins")
		}
		var err error
		workspace, err = ensureSessionWorkspaceContext(ctx, s, intent.Policy.Repository, intent.ForkName, workspaceCommit, intent.SessionID)
		if err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return session.Session{}, err
			}
			return session.Session{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("ensure session workspace: %w", err))
		}
		companions = make([]session.CompanionRepository, 0, len(intent.Companions))
		for _, companion := range intent.Companions {
			resolved, err := ensureSessionCompanionContext(
				ctx, s.store.Root(), intent.SessionID, companion,
			)
			if err != nil {
				return failCreate(
					fmt.Errorf("ensure companion %q: %w", companion.Name, err),
				)
			}
			companions = append(companions, resolved)
		}
		// Network admission runs on the HOST, once, now that the workspace this session will mount
		// exists: it resolves the posture from the operator policy against the project's remembered
		// approval and freezes the exact policy every run of this session will enforce. A refusal —
		// no approval, no host setup, a policy that disagrees with the remembered posture — fails
		// the create with its own reason instead of quietly creating an open session.
		network, err := s.admitSessionNetwork(intent.Policy, workspace.Path, intent.ForkName, companions)
		if err != nil {
			return failCreate(&session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()})
		}
		if intent.Policy.Mode.Restricted() && network.Mode == egress.Filtered {
			// The policy file refused an explicit filtered posture at load; this is the project's
			// remembered one, which can change between the load and this create.
			return failCreate(&session.Error{Code: session.CodeNetworkUnavailable, Detail: fmt.Sprintf(
				"policy %q resolves to filtered networking on this host, which a %s session is not qualified under — set egress.mode to open or none",
				intent.Policy.Name, intent.Policy.Mode)})
		}
		createReq.Repository, createReq.Workspace, createReq.ForkName = intent.Policy.Repository, workspace.Path, intent.ForkName
		createReq.ForkGeneration = string(workspace.Fork.Generation)
		createReq.BaseCommit, createReq.Source, createReq.Companions = intent.BaseCommit, session.CloneSourceBinding(intent.SourceBinding), companions
		createReq.RepositoryFreshness = append([]session.RepositoryFreshnessReceipt(nil), intent.RepositoryFreshness...)
		createReq.NetworkMode, createReq.NetworkFingerprint = string(network.Mode), network.Fingerprint
		createReq.NetworkQualification = network.Qualification
	}
	latest, err := s.store.GetOperationByID(ctx, op.ID)
	if err != nil {
		return session.Session{}, err
	}
	if latest.State != session.OperationRunning {
		return s.replayCreateOperation(ctx, latest)
	}
	if latest.Method != "CreateRemoteSession" || !bytes.Equal(latest.Result, expectedIntent) {
		return session.Session{}, session.ErrOperationIntentConflict
	}
	sess, err := s.store.CompleteCreateSessionOperation(ctx, latest, createReq)
	if err != nil {
		receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		current, readErr := s.store.GetOperationByID(receiptCtx, op.ID)
		if readErr == nil && current.State == session.OperationSucceeded {
			return s.replayCreateOperation(receiptCtx, current)
		}
		if sessionExisted && session.CodeOf(err) == session.CodeOperationIntentConflict &&
			readErr == nil && current.State == session.OperationRunning &&
			current.Method == "CreateRemoteSession" && bytes.Equal(current.Result, expectedIntent) {
			return session.Session{}, s.makeOperationUncertain(
				receiptCtx, current, "existing session conflicts with remote create intent",
			)
		}
		if sessionExisted || readErr != nil || current.State != session.OperationRunning ||
			current.Method != "CreateRemoteSession" || !bytes.Equal(current.Result, expectedIntent) {
			return session.Session{}, errors.Join(err, readErr)
		}
		return failCreate(err)
	}
	return sess, nil
}

func (s *Service) pinCreateIntent(
	ctx context.Context,
	op session.Operation,
	intent sessionCreateIntent,
) (sessionCreateIntent, error) {
	if s.testBeforeCreatePin != nil {
		if err := s.testBeforeCreatePin(); err != nil {
			return sessionCreateIntent{}, err
		}
	}
	if len(intent.Companions) != len(intent.Policy.Companions) {
		return sessionCreateIntent{}, errors.New("create operation companion intent is incomplete")
	}
	for index, companion := range intent.Policy.Companions {
		bound := intent.Companions[index]
		workspace, err := sessionCompanionWorkspace(
			s.store.Root(), intent.SessionID, companion.Name,
		)
		if err != nil {
			return sessionCreateIntent{}, err
		}
		if bound.Name != companion.Name || bound.Repository != companion.Repository ||
			bound.Workspace != workspace || bound.BaseCommit != "" {
			return sessionCreateIntent{}, errors.New("create operation companion intent does not match policy")
		}
	}
	if intent.Source == nil {
		return sessionCreateIntent{}, errors.New("create operation intent names no source")
	}
	pins, err := pinSessionPolicySources(ctx, intent.Policy, *intent.Source)
	if err != nil {
		return sessionCreateIntent{}, err
	}
	intent.BaseCommit = pins.creationBase
	intent.WorkspaceCommit = pins.workspaceHead
	intent.SourceBinding = pins.binding
	intent.RepositoryFreshness = append([]session.RepositoryFreshnessReceipt(nil), pins.receipts...)
	for index := range intent.Companions {
		intent.Companions[index].BaseCommit = pins.companions[index]
	}
	next, err := json.Marshal(intent)
	if err != nil {
		return sessionCreateIntent{}, err
	}
	if err := s.store.ReplaceOperationIntent(ctx, op.ID, op.Result, next); err != nil {
		return sessionCreateIntent{}, err
	}
	return intent, nil
}

func validSessionIntentTargets(targets []agents.Target) bool {
	if len(targets) == 0 || len(targets) > sessionPolicyMaxTargets {
		return false
	}
	for _, target := range targets {
		parsed, err := agents.ParseTarget(target.String())
		if err != nil || len(parsed.Accounts) > 1 {
			return false
		}
	}
	return true
}

func (s *Service) rejectCreateIntent(
	ctx context.Context,
	operationID string,
	detail string,
) (session.Session, error) {
	op, _ := s.store.GetOperationByID(context.WithoutCancel(ctx), operationID)
	if op.ID == "" {
		op.ID = operationID
	}
	return session.Session{}, s.makeOperationUncertain(ctx, op, detail)
}

func (s *Service) makeOperationUncertain(
	ctx context.Context,
	op session.Operation,
	detail string,
) error {
	receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := s.store.MarkOperationUncertain(receiptCtx, op.ID); err != nil {
		return &serviceOperationError{
			operationID: op.ID,
			err:         fmt.Errorf("mark operation uncertain: %w", err),
		}
	}
	detail = s.operationalErrorDetail(errors.New(detail))
	s.log.Warn("session operation intent rejected",
		"operation_id", op.ID, "method", op.Method,
		"resource_type", op.ResourceType, "resource_id", op.ResourceID,
		"error_code", session.CodeOperationUncertain, "error_detail", detail,
	)
	return &serviceOperationError{
		operationID: op.ID,
		err: &session.Error{
			Code: session.CodeOperationUncertain, Detail: detail,
		},
	}
}

func rollbackSessionCreate(
	workspace sessionWorkspace,
	companions []session.CompanionRepository,
) error {
	var cleanupErrors []error
	for index := len(companions) - 1; index >= 0; index-- {
		companion := companions[index]
		plan, err := planSessionCompanionDiscard(companion)
		if err == nil {
			err = discardSessionCompanion(plan)
		}
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf(
				"companion %q: %w", companion.Name, err,
			))
		}
	}
	plan, err := planSessionWorkspaceDiscardAtParent(
		workspace.Repo, workspace.Path, workspace.BaseCommit, false, false,
	)
	if err == nil {
		err = discardSessionWorkspace(plan)
	}
	if err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf(
			"primary workspace: %w", err,
		))
	}
	return errors.Join(cleanupErrors...)
}

func (s *Service) failServiceOperation(ctx context.Context, id string, err error) error {
	code := session.CodeOf(err)
	if code == "" {
		code = session.CodeInternal
	}
	detail := s.operationalErrorDetail(err)
	storedDetail := detail
	if code == session.CodeRepositoryUnavailable {
		var typed *session.Error
		if errors.As(err, &typed) {
			storedDetail = typed.Detail
		}
	}
	receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	op, _ := s.store.GetOperationByID(receiptCtx, id)
	failErr := s.store.FailOperation(receiptCtx, id, code, storedDetail)
	attributes := []any{
		"operation_id", id, "method", op.Method,
		"resource_type", op.ResourceType, "resource_id", op.ResourceID,
		"error_code", code, "error_detail", detail,
	}
	if failErr != nil {
		attributes = append(attributes, "receipt_error", s.operationalErrorDetail(failErr))
	}
	s.log.Error("session operation failed", attributes...)
	return &serviceOperationError{operationID: id, err: err}
}

// correlateOperationError handles mutations whose store transaction owns the
// operation reservation and failure receipt. It preserves the store's typed
// error while adding the same correlation and redacted log surface as service-
// owned operations.
func (s *Service) correlateOperationError(
	ctx context.Context,
	key string,
	err error,
) error {
	if err == nil || key == "" {
		return err
	}
	receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	op, getErr := s.store.GetOperation(receiptCtx, key)
	if getErr != nil || op.ID == "" {
		return err
	}
	code := session.CodeOf(err)
	if code == "" {
		code = session.CodeInternal
	}
	detail := s.operationalErrorDetail(err)
	if op.State == session.OperationFailed {
		code = op.ErrorCode
		detail = s.operationalErrorDetail(errors.New(op.ErrorDetail))
	}
	s.log.Error("session operation failed",
		"operation_id", op.ID, "method", op.Method,
		"resource_type", op.ResourceType, "resource_id", op.ResourceID,
		"error_code", code, "error_detail", detail,
	)
	return wrapServiceOperationError(op.ID, err)
}

func wrapServiceOperationError(operationID string, err error) error {
	if err == nil || operationID == "" {
		return err
	}
	var correlated interface{ OperationID() string }
	if errors.As(err, &correlated) {
		return err
	}
	return &serviceOperationError{operationID: operationID, err: err}
}

type serviceOperationError struct {
	operationID string
	err         error
}

func (e *serviceOperationError) Error() string       { return e.err.Error() }
func (e *serviceOperationError) Unwrap() error       { return e.err }
func (e *serviceOperationError) OperationID() string { return e.operationID }

func (s *Service) operationalErrorDetail(err error) string {
	if err == nil {
		return "session operation failed"
	}
	detail := err.Error()
	detail = strings.ToValidUTF8(detail, "�")
	if s.stateRoot != "" {
		detail = strings.ReplaceAll(detail, s.stateRoot, "<state-root>")
	}
	for name, policy := range s.policies {
		detail = strings.ReplaceAll(detail, policy.Repository, "<repository:"+name+">")
		for _, companion := range policy.Companions {
			detail = strings.ReplaceAll(
				detail, companion.Repository, "<repository:"+companion.Name+">")
		}
	}
	lines := strings.Split(detail, "\n")
	for index, line := range lines {
		if len(box.ScanSecrets(line)) > 0 {
			lines[index] = "<redacted secret-bearing diagnostic line>"
		}
	}
	detail = strings.Join(lines, "\n")
	if len(detail) > session.MaxErrorDetailBytes {
		detail = detail[:session.MaxErrorDetailBytes]
		for !utf8.ValidString(detail) {
			detail = detail[:len(detail)-1]
		}
	}
	return detail
}

func utf8SessionText(value string) bool {
	return !strings.ContainsRune(value, '\x00') && utf8.ValidString(value)
}

func (s *Service) GetSession(ctx context.Context, id string) (session.Session, error) {
	return s.store.GetSession(ctx, id)
}

func (s *Service) PrepareSession(ctx context.Context, id string, expectedRevision int64) (session.Session, error) {
	unlock := s.lockSessionRuntime(id)
	defer unlock()
	bound, err := s.store.GetSession(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	if err := validateSessionForkAuthority(ctx, bound); err != nil {
		return session.Session{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	if bound.Revision != expectedRevision {
		if inspector, ok := s.runner.(sessionRunnerWarmInspector); ok && inspector.WarmSessionReady(bound) {
			return bound, nil
		}
		return session.Session{}, &session.Error{
			Code:   session.CodeRevisionConflict,
			Detail: fmt.Sprintf("expected revision %d, current revision %d", expectedRevision, bound.Revision),
		}
	}
	if bound.State != session.SessionOpen || bound.Activity != session.ActivityParked ||
		bound.ActiveTurnID != "" || bound.QueuedTurnCount != 0 {
		return session.Session{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: "session must be open and idle before it can be prepared"}
	}
	policy, ok := s.policies[bound.Policy]
	if !ok || resolvedSessionPolicyDigest(policy) != bound.PolicyDigest {
		return session.Session{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: "session policy no longer matches the operator policy"}
	}
	if policy.WarmIdleTimeout <= 0 {
		return session.Session{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: "session policy does not enable warm execution"}
	}
	preparer, ok := s.runner.(sessionRunnerPreparer)
	if !ok {
		return session.Session{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: "session runner does not support warm execution"}
	}
	prepareCtx, cancel := context.WithTimeout(ctx, policy.TurnTimeout)
	defer cancel()
	if err := preparer.PrepareSession(prepareCtx, bound, policy.WarmIdleTimeout); err != nil {
		return session.Session{}, err
	}
	// Preparing a warm runtime changes host state without touching the durable
	// session revision. Any earlier "runtime is clean" proof is now obsolete;
	// after expiry the janitor must inspect and retry best-effort teardown.
	s.invalidateRuntimeCleanup(id)
	return s.store.GetSession(ctx, id)
}

func (s *Service) ListSessions(ctx context.Context, limit int) ([]session.Session, error) {
	return s.store.ListSessions(ctx, limit)
}

// SubmitTurn admits a turn without taking the session's runtime lock: admission is one store
// transaction fenced by expected_revision, and the fork-authority check takes its own lock. The
// runtime lock belongs to whoever runs, reviews, or cleans up the workspace — a submit made while a
// turn is executing must queue behind it, never wait on the line for it to finish.
func (s *Service) SubmitTurn(ctx context.Context, key string, req session.SubmitTurnRequest) (session.Turn, error) {
	if err := s.validateTurnEscalation(ctx, req); err != nil {
		return session.Turn{}, err
	}
	if s.restoreInProgress(req.SessionID) {
		// Refused before any receipt is journaled, so the same key succeeds once the restore is
		// done and the turn then runs on the restored workspace it was meant for.
		return session.Turn{}, &session.Error{Code: session.CodeInvalidSessionState,
			Detail: "a workspace restore is in progress on this session; retry once it completes"}
	}
	turn, err := s.store.SubmitTurn(ctx, key, req)
	if err == nil {
		s.schedule(req.SessionID)
	} else {
		err = s.correlateOperationError(ctx, key, err)
	}
	return turn, err
}

// validateTurnEscalation resolves a turn's escalation floor against the ladder it names, at
// admission — this is the layer that knows the policy, so it is the only one whose refusal can
// say how many rungs there are. The runner refuses an unresolvable floor as well, but a caller
// that mistyped a rung index should hear it while it is still on the line, not a minute later as
// a failed turn.
func (s *Service) validateTurnEscalation(ctx context.Context, req session.SubmitTurnRequest) error {
	bound, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		// Not this check's failure to report: the store admits the turn against its own
		// canonical errors and records the operation a retry reads back.
		return nil
	}
	if err := validateSessionForkAuthority(ctx, bound); err != nil {
		return &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	if bound.NetworkMode == string(egress.None) && req.ResponderBinding != nil {
		return &session.Error{Code: session.CodeInvalidRequest, Detail: responderNeedsNetwork}
	}
	if err := validateRestrictedTurn(bound, req); err != nil {
		return err
	}
	if req.MinTargetIndex <= 0 {
		return nil
	}
	policy, ok := s.policies[bound.Policy]
	if !ok {
		return &session.Error{
			Code:   session.CodeInvalidRequest,
			Detail: "this session's policy is no longer configured, so min_target_index names no rung",
		}
	}
	if req.MinTargetIndex >= len(policy.Targets) {
		return &session.Error{Code: session.CodeInvalidRequest, Detail: fmt.Sprintf(
			"min_target_index %d is not a rung of this session's %d-rung target ladder",
			req.MinTargetIndex, len(policy.Targets),
		)}
	}
	return nil
}

// responderNeedsNetwork is the one sentence both admissions use: a Responder endpoint is reached
// by URL, and an offline box reaches none.
const responderNeedsNetwork = "an offline session binds no Responder MCP endpoint — its box reaches no server by URL"

// validateRestrictedTurn refuses, at admission, the two turn options a restricted session cannot
// honor. Each of its turns runs in a fresh box whose provider history dies with it, so there is
// no native session for a caller's semantic rejection to re-prompt; and a bare session runs no
// tool, so a Responder state endpoint bound to its turn would be authority nothing can use.
func validateRestrictedTurn(bound session.Session, req session.SubmitTurnRequest) error {
	if !agents.ExecutionMode(bound.Mode).Restricted() {
		return nil
	}
	if req.OutputContract != nil && req.OutputContract.RequireSemanticValidation {
		return &session.Error{Code: session.CodeInvalidRequest, Detail: fmt.Sprintf(
			"a %s session keeps no provider history to re-prompt, so require_semantic_validation is not available; validate the completed turn's assistant message instead", bound.Mode)}
	}
	if bound.Mode == string(agents.ModeBare) && req.ResponderBinding != nil {
		return &session.Error{Code: session.CodeInvalidRequest, Detail: "a bare session binds no Responder MCP endpoint — it runs no tool"}
	}
	return nil
}

func (s *Service) GetTurn(ctx context.Context, sessionID, turnID string) (session.Turn, error) {
	return s.store.GetTurn(ctx, sessionID, turnID)
}

func (s *Service) AcceptTurnCandidate(ctx context.Context, key, sessionID, turnID, digest string) (session.Turn, error) {
	request := struct {
		SessionID, TurnID, Digest, Verdict string
	}{sessionID, turnID, digest, "accept"}
	turn, err := s.validateTurnCandidateOperation(ctx, key, request, func() (session.Turn, error) {
		unlock, err := s.lockAndReapAwaitingCandidateRuntime(ctx, sessionID, turnID, digest)
		if err != nil {
			return session.Turn{}, err
		}
		defer unlock()
		return s.store.CompleteTurn(ctx, session.CompleteTurnRequest{
			SessionID: sessionID, TurnID: turnID, CandidateSHA256: digest,
		})
	})
	if err == nil {
		s.schedule(sessionID)
	}
	return turn, err
}

func (s *Service) RejectTurnCandidate(ctx context.Context, key string, req session.RejectTurnCandidateRequest) (session.Turn, error) {
	request := struct {
		Request session.RejectTurnCandidateRequest
		Verdict string
	}{req, "reject"}
	turn, err := s.validateTurnCandidateOperation(ctx, key, request, func() (session.Turn, error) {
		unlock, err := s.lockAndReapAwaitingCandidateRuntime(ctx, req.SessionID, req.TurnID, req.CandidateSHA256)
		if err != nil {
			return session.Turn{}, err
		}
		defer unlock()
		return s.store.RejectTurnCandidate(ctx, req)
	})
	if err == nil {
		s.schedule(req.SessionID)
	}
	return turn, err
}

func (s *Service) validateTurnCandidateOperation(
	ctx context.Context,
	key string,
	request any,
	apply func() (session.Turn, error),
) (session.Turn, error) {
	unlock := s.lockOperation(key)
	defer unlock()
	op, replay, err := s.store.ReserveOperation(ctx, "ValidateTurnCandidate", key, request)
	if err != nil {
		return session.Turn{}, err
	}
	if replay {
		switch op.State {
		case session.OperationSucceeded:
			turn, err := session.DecodeTurnOperationResult(op.Result)
			if err != nil {
				return session.Turn{}, errors.New("decode semantic validation operation result")
			}
			return turn, nil
		case session.OperationFailed:
			return session.Turn{}, wrapServiceOperationError(op.ID,
				&session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail})
		}
	}
	turn, err := apply()
	if err != nil {
		return session.Turn{}, s.failServiceOperation(ctx, op.ID, err)
	}
	result, err := session.EncodeTurnOperationResult(turn)
	if err != nil {
		return session.Turn{}, err
	}
	if err := s.store.CompleteOperation(ctx, op.ID, "turn_validation", turn.ID, result); err != nil {
		return session.Turn{}, err
	}
	return turn, nil
}

// lockAndReapAwaitingCandidateRuntime serializes the caller's decision with
// the provider runtime that produced the candidate. The durable transition is
// allowed only after exact runtime cleanup succeeds; otherwise moving the turn
// out of awaiting_validation would erase the janitor's last ownership signal.
func (s *Service) lockAndReapAwaitingCandidateRuntime(
	ctx context.Context,
	sessionID, turnID, candidateSHA256 string,
) (func(), error) {
	unlock := s.lockSessionRuntime(sessionID)
	fail := func(err error) (func(), error) {
		unlock()
		return nil, err
	}
	bound, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return fail(err)
	}
	if err := validateSessionForkAuthority(ctx, bound); err != nil {
		return fail(&session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()})
	}
	turn, err := s.store.GetTurn(ctx, sessionID, turnID)
	if err != nil {
		return fail(err)
	}
	if bound.Activity != session.ActivityRunning || bound.ActiveTurnID != turn.ID ||
		turn.State != session.TurnAwaitingValidation || turn.Candidate == nil {
		return fail(&session.Error{Code: session.CodeTurnNotRunnable, Detail: "turn does not await semantic validation"})
	}
	if candidateSHA256 != "" && turn.CandidateSHA256 != candidateSHA256 {
		return fail(&session.Error{Code: session.CodeRevisionConflict, Detail: "semantic candidate digest is stale"})
	}
	stamp := runtimeCleanupStampFor(bound, &turn)
	if s.runtimeCleanupMatches(sessionID, stamp) {
		return unlock, nil
	}
	reaper, ok := s.runner.(sessionRunnerTurnReaper)
	if !ok {
		return fail(&session.Error{Code: sessionACPCleanupError, Detail: "runtime cleanup is unavailable"})
	}
	if err := reaper.ReapInterruptedTurn(ctx, bound, turn); err != nil {
		return fail(&session.Error{
			Code:   sessionACPCleanupError,
			Detail: sessionACPBoundedDetail("runtime cleanup failed", err.Error()),
		})
	}
	s.markRuntimeCleanupDone(sessionID, stamp)
	return unlock, nil
}

func (s *Service) ListTurns(ctx context.Context, sessionID string, afterOrdinal int64, limit int) ([]session.Turn, error) {
	return s.store.ListTurns(ctx, sessionID, afterOrdinal, limit)
}

func (s *Service) GetOutputArtifact(ctx context.Context, sessionID, turnID, artifactID string) (session.OutputArtifact, error) {
	return s.store.GetOutputArtifact(ctx, sessionID, turnID, artifactID)
}

func (s *Service) ListEvents(ctx context.Context, sessionID string, after int64, limit int) ([]session.Event, error) {
	return s.store.ListEvents(ctx, sessionID, after, limit)
}

func (s *Service) ExtendBudget(ctx context.Context, key string, req session.ExtendBudgetRequest) (session.Session, error) {
	bound, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return session.Session{}, err
	}
	if err := requireSessionForkAuthority(bound); err != nil {
		return session.Session{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	sess, err := s.store.ExtendBudget(ctx, key, req)
	return sess, s.correlateOperationError(ctx, key, err)
}

func (s *Service) Close(ctx context.Context, key string, req session.CloseSessionRequest) (session.Session, error) {
	unlock := s.lockSessionRuntime(req.SessionID)
	defer unlock()
	current, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return session.Session{}, err
	}
	if err := validateSessionForkAuthority(ctx, current); err != nil {
		return session.Session{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	if current.State == session.SessionClosed {
		closed, closeErr := s.store.CloseSession(ctx, key, req)
		return closed, s.correlateOperationError(ctx, key, closeErr)
	}
	if current.Revision != req.ExpectedRevision {
		return session.Session{}, session.ErrRevisionConflict
	}
	if cleaner, ok := s.runner.(sessionRunnerClosedCleaner); ok {
		if err := cleaner.CleanupClosedSession(ctx, current); err != nil {
			return session.Session{}, err
		}
	} else if cleaner, ok := s.runner.(sessionRunnerRuntimeCleaner); ok {
		if err := cleaner.CleanupSession(ctx, current); err != nil {
			return session.Session{}, err
		}
	}
	closed, closeErr := s.store.CloseSession(ctx, key, req)
	return closed, s.correlateOperationError(ctx, key, closeErr)
}

func (s *Service) CloseSession(ctx context.Context, key string, req session.CloseSessionRequest) (session.Session, error) {
	return s.Close(ctx, key, req)
}

func (s *Service) GetChanges(ctx context.Context, sessionID string) (WorkspaceChanges, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return WorkspaceChanges{}, err
	}
	if err := requireSessionWorkspace(sess); err != nil {
		return WorkspaceChanges{}, err
	}
	if err := validateSessionForkAuthority(ctx, sess); err != nil {
		return WorkspaceChanges{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	parentHead, err := s.pinCurrentSessionParent(ctx, sess)
	if err != nil {
		return WorkspaceChanges{}, err
	}
	changes, err := inspectSessionChangesPageAtParent(
		sess.Repository, sess.Workspace, sess.BaseCommit, parentHead, 0, sess.MaxPatchBytes,
	)
	if err != nil {
		return WorkspaceChanges{}, err
	}
	if sess.Source != nil {
		changes.AdmittedSourceTree, err = sessionWorkspaceTree(
			sess.Workspace, sess.Source.SelectedCommit,
		)
	}
	return changes, err
}

func (s *Service) GetChangesPage(
	ctx context.Context,
	sessionID string,
	patchOffset int64,
	patchLimit int,
) (WorkspaceChanges, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return WorkspaceChanges{}, err
	}
	if err := requireSessionWorkspace(sess); err != nil {
		return WorkspaceChanges{}, err
	}
	if err := validateSessionForkAuthority(ctx, sess); err != nil {
		return WorkspaceChanges{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	if patchLimit < 1 || patchLimit > sess.MaxPatchBytes {
		return WorkspaceChanges{}, &session.Error{
			Code: session.CodeInvalidRequest,
			Detail: fmt.Sprintf(
				"patch limit must be between 1 and %d bytes",
				sess.MaxPatchBytes,
			),
		}
	}
	parentHead, err := s.pinCurrentSessionParent(ctx, sess)
	if err != nil {
		return WorkspaceChanges{}, err
	}
	changes, err := inspectSessionChangesPageAtParent(
		sess.Repository,
		sess.Workspace,
		sess.BaseCommit,
		parentHead,
		patchOffset,
		patchLimit,
	)
	if err != nil {
		return WorkspaceChanges{}, err
	}
	if sess.Source != nil {
		changes.AdmittedSourceTree, err = sessionWorkspaceTree(
			sess.Workspace, sess.Source.SelectedCommit,
		)
	}
	return changes, err
}

type PlanDiscardRequest struct {
	SessionID        string `json:"session_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	AcceptDirty      bool   `json:"accept_dirty"`
	AcceptUnmerged   bool   `json:"accept_unmerged"`
}

type DiscardPlan struct {
	SessionID  string                        `json:"session_id"`
	Revision   int64                         `json:"revision"`
	Workspace  WorkspaceDiscardPlan          `json:"workspace"`
	Companions []sessionCompanionDiscardPlan `json:"companions,omitempty"`
}

type PlanDiscardResult struct {
	OperationID string      `json:"operation_id"`
	Plan        DiscardPlan `json:"plan"`
}

func (s *Service) PlanDiscard(ctx context.Context, key string, req PlanDiscardRequest) (PlanDiscardResult, error) {
	unlock := s.lockOperation(key)
	defer unlock()

	op, replay, err := s.store.ReserveOperation(ctx, "PlanDiscard", key, req)
	if err != nil {
		return PlanDiscardResult{}, err
	}
	if replay {
		if op.State == session.OperationReserved {
			return s.executePlanDiscard(ctx, op, req)
		}
		result, err := replayPlanDiscard(op)
		if err != nil || op.State != session.OperationRunning {
			return result, err
		}
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return PlanDiscardResult{}, marshalErr
		}
		if completeErr := s.store.CompleteOperation(ctx, op.ID, "discard_plan", result.Plan.SessionID, data); completeErr != nil {
			return PlanDiscardResult{}, completeErr
		}
		return result, nil
	}
	return s.executePlanDiscard(ctx, op, req)
}

func (s *Service) executePlanDiscard(ctx context.Context, op session.Operation, req PlanDiscardRequest) (PlanDiscardResult, error) {
	if req.SessionID == "" || req.ExpectedRevision <= 0 {
		return PlanDiscardResult{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidRequest, Detail: "session and revision are required"})
	}
	sess, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return PlanDiscardResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := validateSessionForkAuthorityForDiscardPlan(ctx, sess); err != nil {
		return PlanDiscardResult{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()})
	}
	if sess.Revision != req.ExpectedRevision || sess.State != session.SessionClosed || sess.ActiveTurnID != "" || sess.QueuedTurnCount != 0 {
		return PlanDiscardResult{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidSessionState, Detail: "discard planning requires a closed idle session"})
	}
	if sess.Workspace == "" {
		// Nothing to plan against: a bare session's discard removes its private state and
		// retires the record. The plan still pins the revision the discard must find.
		return s.completePlanDiscard(ctx, op, PlanDiscardResult{
			OperationID: op.ID, Plan: DiscardPlan{SessionID: sess.ID, Revision: sess.Revision},
		})
	}
	parentHead, err := s.pinDiscardSessionParent(ctx, sess)
	if err != nil {
		return PlanDiscardResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	plan, err := planSessionWorkspaceDiscardAtParent(
		sess.Repository, sess.Workspace, parentHead, req.AcceptDirty, req.AcceptUnmerged,
	)
	if err != nil {
		return PlanDiscardResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if plan.Running {
		return PlanDiscardResult{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidSessionState, Detail: "workspace has active or pending work"})
	}
	companions := make([]sessionCompanionDiscardPlan, 0, len(sess.Companions))
	for _, companion := range sess.Companions {
		companionPlan, err := planSessionCompanionDiscardContext(ctx, companion)
		if err != nil {
			return PlanDiscardResult{}, s.failServiceOperation(
				ctx, op.ID,
				fmt.Errorf("plan companion %q discard: %w", companion.Name, err),
			)
		}
		companions = append(companions, companionPlan)
	}
	return s.completePlanDiscard(ctx, op, PlanDiscardResult{
		OperationID: op.ID,
		Plan: DiscardPlan{
			SessionID: sess.ID, Revision: sess.Revision,
			Workspace: plan, Companions: companions,
		},
	})
}

func (s *Service) completePlanDiscard(ctx context.Context, op session.Operation, result PlanDiscardResult) (PlanDiscardResult, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return PlanDiscardResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := s.store.MarkOperationRunning(ctx, op.ID, data); err != nil {
		return PlanDiscardResult{}, err
	}
	if err := s.store.CompleteOperation(ctx, op.ID, "discard_plan", result.Plan.SessionID, data); err != nil {
		return PlanDiscardResult{}, err
	}
	return result, nil
}

func replayPlanDiscard(op session.Operation) (PlanDiscardResult, error) {
	if op.State == session.OperationFailed {
		return PlanDiscardResult{}, wrapServiceOperationError(op.ID,
			&session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail})
	}
	if op.State != session.OperationSucceeded && op.State != session.OperationRunning {
		return PlanDiscardResult{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
	}
	var result PlanDiscardResult
	if err := json.Unmarshal(op.Result, &result); err != nil {
		return PlanDiscardResult{}, wrapServiceOperationError(op.ID, err)
	}
	return result, nil
}

type DiscardRequest struct {
	PlanOperationID string `json:"plan_operation_id"`

	// RetireQuarantined retires a session the daemon quarantined at start — a legacy record with
	// no fork ownership proof, or one whose workspace is gone — as a discarded tombstone WITHOUT
	// touching a workspace or service Coop cannot prove it owns. SessionID and ExpectedRevision
	// name the exact record; the ordinary plan-then-discard path is refused for such sessions.
	RetireQuarantined bool   `json:"retire_quarantined,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	ExpectedRevision  int64  `json:"expected_revision,omitempty"`
}

func (s *Service) Discard(ctx context.Context, key string, req DiscardRequest) (session.Session, error) {
	unlock := s.lockOperation(key)
	defer unlock()

	op, replay, err := s.store.ReserveOperation(ctx, "Discard", key, req)
	if err != nil {
		return session.Session{}, err
	}
	if replay {
		if op.State == session.OperationRunning {
			var intent discardIntent
			if err := json.Unmarshal(op.Result, &intent); err != nil {
				return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
			}
			return s.executeDiscard(ctx, op, intent.Plan)
		}
		if op.State == session.OperationReserved {
			return s.executeDiscardRequest(ctx, op, req)
		}
		return replaySessionOperation(op)
	}
	return s.executeDiscardRequest(ctx, op, req)
}

type discardIntent struct {
	Plan PlanDiscardResult `json:"plan"`
}

// executeRetireQuarantined tombstones a quarantined session's record. Quarantine means Coop could
// not prove workspace authority at start, so nothing on disk is touched: the workspace, services,
// and private ACP state stay exactly where the operator can inspect them. A session that is not
// quarantined must go through plan-then-discard, which does hold that authority.
func (s *Service) executeRetireQuarantined(ctx context.Context, op session.Operation, req DiscardRequest) (session.Session, error) {
	if req.PlanOperationID != "" || req.SessionID == "" || req.ExpectedRevision <= 0 {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidRequest, Detail: "retiring a quarantined session takes session_id and expected_revision, not a plan",
		})
	}
	sess, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if !s.sessionQuarantined(sess.ID) {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidSessionState, Detail: "session is not quarantined; plan and execute an ordinary discard",
		})
	}
	if sess.Revision != req.ExpectedRevision {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeRevisionConflict, Detail: "session revision changed",
		})
	}
	sess, err = s.store.RetireQuarantinedSession(ctx, sess.ID)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	s.mu.Lock()
	delete(s.quarantined, sess.ID)
	s.mu.Unlock()
	return s.completeDiscardOperation(ctx, op.ID, sess)
}

func (s *Service) executeDiscardRequest(ctx context.Context, op session.Operation, req DiscardRequest) (session.Session, error) {
	if req.RetireQuarantined {
		return s.executeRetireQuarantined(ctx, op, req)
	}
	if req.PlanOperationID == "" {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidRequest, Detail: "discard plan operation id is required"})
	}
	planOp, err := s.store.GetOperationByID(ctx, req.PlanOperationID)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if planOp.Method != "PlanDiscard" || planOp.State != session.OperationSucceeded {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, session.ErrOperationUncertain)
	}
	var planned PlanDiscardResult
	if err := json.Unmarshal(planOp.Result, &planned); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, session.ErrOperationUncertain)
	}
	return s.executeDiscard(ctx, op, planned)
}

func (s *Service) executeDiscard(ctx context.Context, op session.Operation, planned PlanDiscardResult) (session.Session, error) {
	intentData, err := json.Marshal(discardIntent{Plan: planned})
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	sess, err := s.store.GetSession(ctx, planned.Plan.SessionID)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := requireSessionForkAuthority(sess); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()})
	}
	if err := validateDiscardSessionBinding(sess, planned.Plan); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeDiscardPlanStale, Detail: boundedSessionServiceError(err)})
	}
	if op.State == session.OperationReserved {
		if err := s.store.MarkOperationRunning(ctx, op.ID, intentData); err != nil {
			return session.Session{}, err
		}
	}
	if sess.State == session.SessionDiscarded {
		if err := s.removeSessionReviewArtifacts(ctx, sess.ID); err != nil {
			return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
		}
		completed, err := s.completeDiscardOperation(ctx, op.ID, sess)
		if err != nil {
			return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
		}
		return completed, nil
	}
	if sess.Revision != planned.Plan.Revision || sess.State != session.SessionClosed || sess.ActiveTurnID != "" || sess.QueuedTurnCount != 0 {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeDiscardPlanStale, Detail: "discard plan no longer matches session state"})
	}
	if sess.Workspace == "" {
		return s.retireWorkspacelessSession(ctx, op, sess)
	}
	workspacePlan := planned.Plan.Workspace
	unlockWorkspace, err := forkspace.LockStateContext(ctx, workspacePlan.Repo, workspacePlan.Name)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("lock session workspace discard: %w", err))
	}
	preflight, err := validateSessionWorkspaceDiscardLocked(workspacePlan)
	if err != nil {
		unlockWorkspace()
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeDiscardPlanStale, Detail: boundedSessionServiceError(err)})
	}
	if s.rt.Name != "" {
		if err := box.DownServices(
			s.rt,
			workspacePlan.Workspace,
			workspacePlan.Repo,
			true,
			io.Discard,
			io.Discard,
			append(box.ConfigExposureRoots(s.sourceCfg), s.stateRoot)...,
		); err != nil {
			_ = preflight.close()
			unlockWorkspace()
			return session.Session{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("remove session services: %w", err))
		}
	}
	_ = preflight.close()
	workspaceErr := discardSessionWorkspaceLocked(workspacePlan)
	unlockWorkspace()
	if workspaceErr != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeDiscardPlanStale, Detail: boundedSessionServiceError(workspaceErr)})
	}
	for _, companion := range planned.Plan.Companions {
		if err := discardSessionCompanionContext(ctx, companion); err != nil {
			return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
		}
	}
	if err := removePrivateSessionState(s.store.Root(), planned.Plan.SessionID); err != nil {
		return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
	}
	sess, err = s.store.MarkSessionDiscarded(ctx, planned.Plan.SessionID)
	if err != nil {
		return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
	}
	if err := s.removeSessionReviewArtifacts(ctx, sess.ID); err != nil {
		return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
	}
	completed, err := s.completeDiscardOperation(ctx, op.ID, sess)
	if err != nil {
		return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
	}
	return completed, nil
}

// retireWorkspacelessSession is the discard of a session that never had a workspace (a bare
// session): its private ACP state goes, the record is tombstoned, and no fork, service or
// companion is touched because none exists.
func (s *Service) retireWorkspacelessSession(ctx context.Context, op session.Operation, sess session.Session) (session.Session, error) {
	if err := removePrivateSessionState(s.store.Root(), sess.ID); err != nil {
		return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
	}
	sess, err := s.store.MarkSessionDiscarded(ctx, sess.ID)
	if err != nil {
		return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
	}
	completed, err := s.completeDiscardOperation(ctx, op.ID, sess)
	if err != nil {
		return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
	}
	return completed, nil
}

func validateDiscardSessionBinding(sess session.Session, plan DiscardPlan) error {
	if plan.SessionID != sess.ID || plan.Revision != sess.Revision {
		return errors.New("discard plan does not belong to this session revision")
	}
	if sess.Workspace == "" && sess.Repository == "" && sess.ForkName == "" && sess.ForkGeneration == "" {
		// DeepEqual, not ==: WorkspaceIdentity is deliberately not comparable (see sameDirectory).
		if !reflect.DeepEqual(plan.Workspace, WorkspaceDiscardPlan{}) || len(plan.Companions) != 0 {
			return errors.New("discard plan names a workspace for a session that has none")
		}
		return nil
	}
	workspace := plan.Workspace
	if workspace.Repo != sess.Repository || workspace.Workspace != sess.Workspace || workspace.Name != sess.ForkName {
		return errors.New("discard plan workspace does not match the session binding")
	}
	if sess.ForkGeneration == "" {
		return errLegacySessionForkUnproven
	}
	expected := forkspace.Identity{Name: sess.ForkName, Generation: forkspace.Generation(sess.ForkGeneration)}
	if workspace.Fork == nil || *workspace.Fork != expected {
		return errors.New("discard plan fork generation does not match the session binding")
	}
	if workspace.Reservation == nil || workspace.Reservation.Version != forkspace.WorkspaceReservationVersion ||
		workspace.Reservation.Fork != expected || workspace.Reservation.Kind != forkspace.WorkspaceReservationRemoteSession ||
		workspace.Reservation.OwnerID != sess.ID {
		return errors.New("discard plan reservation does not belong to this session")
	}
	if len(plan.Companions) != len(sess.Companions) {
		return errors.New("discard plan companion set changed")
	}
	for i, companion := range sess.Companions {
		planned := plan.Companions[i]
		if planned.Name != companion.Name || planned.Repo != companion.Repository ||
			planned.Workspace != companion.Workspace || planned.Head != companion.BaseCommit {
			return errors.New("discard plan companion binding changed")
		}
	}
	return nil
}

func (s *Service) removeSessionReviewArtifacts(
	ctx context.Context,
	sessionID string,
) error {
	operationIDs, err := s.store.ListOperationIDsForResource(
		ctx,
		"RunReview",
		"review",
		sessionID,
	)
	if err != nil {
		return err
	}
	root := filepath.Join(s.stateRoot, "review-artifacts")
	for _, operationID := range operationIDs {
		if !validSessionPathComponent(operationID) {
			return errors.New("stored review operation has an invalid artifact identity")
		}
		path := filepath.Join(root, operationID+".diff")
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove review patch artifact: %w", err)
		}
	}
	return nil
}

func replaySessionOperation(op session.Operation) (session.Session, error) {
	if op.State == session.OperationFailed {
		return session.Session{}, wrapServiceOperationError(op.ID,
			&session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail})
	}
	if op.State != session.OperationSucceeded {
		return session.Session{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
	}
	var sess session.Session
	if err := json.Unmarshal(op.Result, &sess); err != nil {
		return session.Session{}, wrapServiceOperationError(op.ID, err)
	}
	if sess.ID == "" {
		return session.Session{}, wrapServiceOperationError(op.ID,
			errors.New("decode session operation result: missing session id"))
	}
	return sess, nil
}

func (s *Service) completeDiscardOperation(ctx context.Context, operationID string, sess session.Session) (session.Session, error) {
	data, err := json.Marshal(sess)
	if err != nil {
		return session.Session{}, err
	}
	if err := s.store.CompleteOperation(ctx, operationID, "session", sess.ID, data); err != nil {
		return session.Session{}, err
	}
	return sess, nil
}

func removePrivateSessionState(stateRoot, sessionID string) error {
	if stateRoot == "" || sessionID == "" || !validSessionPathComponent(sessionID) {
		return &session.Error{Code: session.CodeInvalidRequest, Detail: "invalid private session state path"}
	}
	root := filepath.Join(stateRoot, "acp")
	path := filepath.Join(root, sessionID)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return &session.Error{Code: session.CodeInvalidRequest, Detail: "private session state escaped its root"}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect private session state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("private session state is ambiguous")
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove private session state: %w", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("private session state remains after removal")
		}
		return fmt.Errorf("verify private session state removal: %w", err)
	}
	return nil
}

func (s *Service) CancelTurn(ctx context.Context, key string, req session.CancelTurnRequest) (session.Turn, error) {
	unlock := s.lockOperation(key)
	defer unlock()
	op, replay, err := s.store.ReserveOperation(ctx, "CancelTurn", key, req)
	if err != nil {
		return session.Turn{}, err
	}
	if replay && op.State != session.OperationReserved && op.State != session.OperationRunning {
		return replayCancelOperation(op)
	}
	if req.SessionID == "" || req.TurnID == "" || req.ExpectedRevision <= 0 {
		err := &session.Error{Code: session.CodeInvalidRequest, Detail: "session, turn, and revision are required"}
		return session.Turn{}, s.failCancelOperation(ctx, op, err)
	}
	bound, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return session.Turn{}, s.failCancelOperation(ctx, op, err)
	}
	if err := validateSessionForkAuthority(ctx, bound); err != nil {
		return session.Turn{}, s.failCancelOperation(ctx, op,
			&session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()})
	}
	turn, err := s.store.GetTurn(ctx, req.SessionID, req.TurnID)
	if err != nil {
		return session.Turn{}, s.failCancelOperation(ctx, op, err)
	}
	if sessionTurnTerminal(turn.State) {
		if replay {
			return s.completeObservedCancel(ctx, op, turn)
		}
		err := &session.Error{Code: session.CodeTurnNotRunnable, Detail: "turn is already terminal"}
		return session.Turn{}, s.failCancelOperation(ctx, op, err)
	}
	if req.ExpectedRevision != bound.Revision {
		err := &session.Error{Code: session.CodeRevisionConflict, Detail: "cancellation revision is stale"}
		return session.Turn{}, s.failCancelOperation(ctx, op, err)
	}
	if turn.State == session.TurnQueued || turn.State == session.TurnAwaitingValidation {
		var unlockRuntime func()
		if turn.State == session.TurnAwaitingValidation {
			unlockRuntime, err = s.lockAndReapAwaitingCandidateRuntime(ctx, req.SessionID, req.TurnID, "")
			if err != nil {
				return session.Turn{}, s.failCancelOperation(ctx, op, err)
			}
			defer unlockRuntime()
		}
		cancelled, err := s.store.CancelTurn(ctx, key, req)
		if err == nil {
			s.schedule(req.SessionID)
		} else {
			err = s.correlateOperationError(ctx, key, err)
		}
		return cancelled, err
	}
	if turn.State != session.TurnStarting && turn.State != session.TurnRunning {
		if replay {
			return s.completeObservedCancel(ctx, op, turn)
		}
		err := &session.Error{Code: session.CodeTurnNotRunnable, Detail: "turn is already terminal"}
		return session.Turn{}, s.failCancelOperation(ctx, op, err)
	}
	if op.State == session.OperationReserved {
		intent, marshalErr := json.Marshal(req)
		if marshalErr != nil {
			return session.Turn{}, s.failCancelOperation(ctx, op, marshalErr)
		}
		if err := s.store.MarkOperationRunning(ctx, op.ID, intent); err != nil {
			return session.Turn{}, wrapServiceOperationError(op.ID, err)
		}
		op.State = session.OperationRunning
	}
	s.mu.Lock()
	active := s.active[req.TurnID]
	if active == nil {
		pending := s.pendingCancels[req.TurnID]
		if pending != nil && (pending.key != key || pending.request != req) {
			s.mu.Unlock()
			return session.Turn{}, s.failCancelOperation(ctx, op, session.ErrIdempotencyConflict)
		}
		if pending == nil {
			pending = &pendingSessionCancel{key: key, request: req, ready: make(chan struct{})}
			s.pendingCancels[req.TurnID] = pending
		}
		s.mu.Unlock()
		timer := time.NewTimer(s.stopTimeout)
		defer timer.Stop()
		select {
		case <-pending.ready:
			s.mu.Lock()
			active = s.active[req.TurnID]
			s.mu.Unlock()
			if active == nil {
				return s.uncertainCancelOperation(ctx, op, "active turn finished during cancellation handoff")
			}
		case <-timer.C:
			return session.Turn{}, wrapServiceOperationError(op.ID,
				errors.New("active turn worker did not register before the cancellation deadline"))
		case <-ctx.Done():
			return session.Turn{}, wrapServiceOperationError(op.ID, ctx.Err())
		}
		s.mu.Lock()
	}
	if active.requested {
		if active.key != key || active.request != req {
			s.mu.Unlock()
			return session.Turn{}, s.failCancelOperation(ctx, op, session.ErrIdempotencyConflict)
		}
	} else {
		active.key, active.request, active.requested = key, req, true
		active.cancel()
	}
	done := active.done
	s.mu.Unlock()
	timer := time.NewTimer(s.stopTimeout)
	defer timer.Stop()
	select {
	case <-done:
		observed, err := s.store.GetTurn(context.Background(), req.SessionID, req.TurnID)
		if err != nil {
			return s.uncertainCancelOperation(ctx, op, "active turn cleanup could not be read")
		}
		if sessionTurnTerminal(observed.State) {
			return s.completeObservedCancel(context.Background(), op, observed)
		}
		return s.uncertainCancelOperation(ctx, op, "active turn cleanup finished without a terminal result")
	case <-timer.C:
		return s.uncertainCancelOperation(ctx, op, "active turn cleanup is still pending")
	case <-ctx.Done():
		return s.uncertainCancelOperation(ctx, op, "caller stopped waiting during active turn cleanup")
	}
}

func sessionTurnTerminal(state session.TurnState) bool {
	switch state {
	case session.TurnCompleted, session.TurnFailed, session.TurnCancelled, session.TurnInterrupted, session.TurnBudgetExhausted:
		return true
	default:
		return false
	}
}

func (s *Service) failCancelOperation(ctx context.Context, op session.Operation, err error) error {
	if op.ID != "" {
		return s.failServiceOperation(ctx, op.ID, err)
	}
	return err
}

func (s *Service) uncertainCancelOperation(
	ctx context.Context,
	op session.Operation,
	detail string,
) (session.Turn, error) {
	err := s.makeOperationUncertain(ctx, op, detail)
	if session.CodeOf(err) == session.CodeInvalidRequest {
		latest, getErr := s.store.GetOperationByID(context.WithoutCancel(ctx), op.ID)
		if getErr == nil {
			return replayCancelOperation(latest)
		}
	}
	return session.Turn{}, err
}

func (s *Service) completeObservedCancel(ctx context.Context, op session.Operation, turn session.Turn) (session.Turn, error) {
	data, err := session.EncodeTurnOperationResult(turn)
	if err != nil {
		return session.Turn{}, err
	}
	if err := s.store.CompleteOperation(ctx, op.ID, "turn", turn.ID, data); err != nil {
		latest, getErr := s.store.GetOperationByID(context.Background(), op.ID)
		if getErr == nil && latest.State != session.OperationReserved && latest.State != session.OperationRunning {
			return replayCancelOperation(latest)
		}
		return session.Turn{}, err
	}
	return turn, nil
}

func replayCancelOperation(op session.Operation) (session.Turn, error) {
	if op.State == session.OperationFailed {
		return session.Turn{}, wrapServiceOperationError(op.ID,
			&session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail})
	}
	if op.State != session.OperationSucceeded {
		return session.Turn{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
	}
	turn, err := session.DecodeTurnOperationResult(op.Result)
	if err != nil {
		return session.Turn{}, wrapServiceOperationError(op.ID,
			fmt.Errorf("decode cancel operation result: %w", err))
	}
	return turn, nil
}

func (s *Service) GetOperation(ctx context.Context, key string) (session.Operation, error) {
	return s.store.GetOperation(ctx, key)
}

func (s *Service) GetOperationByID(ctx context.Context, id string) (session.Operation, error) {
	return s.store.GetOperationByID(ctx, id)
}

// FenceCreateRemoteSession and FenceSubmitTurn occupy the target mutation's
// exact ledger identity when host authority is revoked before execution. They
// intentionally return the target operation, not a second cleanup operation.
func (s *Service) FenceCreateRemoteSession(
	ctx context.Context,
	key string,
	req CreateRemoteSessionRequest,
) (session.Operation, error) {
	return s.store.FenceOperation(ctx, "CreateRemoteSession", key, req)
}

func (s *Service) FenceSubmitTurn(
	ctx context.Context,
	key string,
	req session.SubmitTurnRequest,
) (session.Operation, error) {
	return s.store.FenceOperation(ctx, "SubmitTurn", key, req)
}
