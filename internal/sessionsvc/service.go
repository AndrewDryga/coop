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
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/session"
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
)

// Policy is operator-owned authority for one remote session. It is intentionally small:
// repository, target, and resource bounds are not request fields.
type Policy struct {
	Name            string
	Repository      string
	Remote          string
	Branch          string
	Companions      []CompanionPolicy
	Targets         []agents.Target
	OmitEnv         bool
	OmitMCP         bool
	MaxTurns        int
	MaxQueuedTurns  int
	MaxQueuedBytes  int
	TurnTimeout     time.Duration
	WarmIdleTimeout time.Duration
	MaxPatchBytes   int
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
}

type rawSessionPolicy struct {
	Repository      string                      `yaml:"repository"`
	Remote          string                      `yaml:"remote"`
	Branch          string                      `yaml:"branch"`
	Companions      []rawSessionCompanionPolicy `yaml:"companions"`
	Target          yaml.Node                   `yaml:"target"`
	ProjectEnv      *bool                       `yaml:"project_env"`
	ProjectMCP      *bool                       `yaml:"project_mcp"`
	MaxTurns        int                         `yaml:"max_turns"`
	MaxQueuedTurns  int                         `yaml:"max_queued_turns"`
	MaxQueuedBytes  int                         `yaml:"max_queued_bytes"`
	TurnTimeout     string                      `yaml:"turn_timeout"`
	WarmIdleTimeout string                      `yaml:"warm_idle_timeout"`
	MaxPatchBytes   int                         `yaml:"max_patch_bytes"`
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
	return parseSessionPolicies(data, cfg)
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
			return errors.New("session policy ancestry contains a symlink")
		}
		if !info.IsDir() {
			return errors.New("session policy ancestry is not a directory")
		}
		owner, ownerOK := sessionFileOwner(info)
		// A trusted sticky directory such as /tmp lets others create their own entries,
		// but not replace this user's protected descendant.
		trustedSticky := info.Mode()&os.ModeSticky != 0 && ownerOK &&
			(owner == uint64(os.Geteuid()) || owner == 0)
		if info.Mode().Perm()&0o022 != 0 && !trustedSticky {
			return errors.New("session policy ancestry is group/world writable")
		}
		if !ownerOK {
			return errors.New("session policy ancestry owner is unavailable")
		} else if owner != uint64(os.Geteuid()) && owner != 0 {
			return errors.New("session policy ancestry is foreign-owned")
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
	if raw.Repository == "" || !filepath.IsAbs(raw.Repository) || filepath.Clean(raw.Repository) != raw.Repository {
		return Policy{}, errors.New("repository must be an absolute, clean path")
	}
	realRepo, err := realGitRepository(raw.Repository)
	if err != nil {
		return Policy{}, err
	}
	if err := validateSessionRepositorySource(raw.Remote, raw.Branch); err != nil {
		return Policy{}, err
	}
	if len(raw.Companions) > sessionPolicyMaxCompanions {
		return Policy{}, fmt.Errorf(
			"companions are limited to %d repositories",
			sessionPolicyMaxCompanions,
		)
	}
	companions := make([]CompanionPolicy, 0, len(raw.Companions))
	seenNames := make(map[string]bool, len(raw.Companions))
	seenRepositories := map[string]bool{realRepo: true}
	for _, companion := range raw.Companions {
		if !validCompanionRepositoryName(companion.Name) {
			return Policy{}, fmt.Errorf(
				"companion name %q must use 1-48 lowercase letters, numbers, hyphens, or underscores and cannot be primary",
				companion.Name,
			)
		}
		if seenNames[companion.Name] {
			return Policy{}, fmt.Errorf("companion name %q is duplicated", companion.Name)
		}
		if companion.Repository == "" || !filepath.IsAbs(companion.Repository) ||
			filepath.Clean(companion.Repository) != companion.Repository {
			return Policy{}, fmt.Errorf(
				"companion %q repository must be an absolute, clean path",
				companion.Name,
			)
		}
		realCompanion, err := realGitRepository(companion.Repository)
		if err != nil {
			return Policy{}, fmt.Errorf("companion %q: %w", companion.Name, err)
		}
		if err := validateSessionRepositorySource(companion.Remote, companion.Branch); err != nil {
			return Policy{}, fmt.Errorf("companion %q: %w", companion.Name, err)
		}
		if seenRepositories[realCompanion] {
			return Policy{}, fmt.Errorf(
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
	var warmIdleTimeout time.Duration
	if raw.WarmIdleTimeout != "" {
		warmIdleTimeout, err = time.ParseDuration(raw.WarmIdleTimeout)
		if err != nil || warmIdleTimeout <= 0 || warmIdleTimeout > sessionPolicyMaxWarmIdleTimeout {
			return Policy{}, fmt.Errorf("warm_idle_timeout must be positive and no longer than %s", sessionPolicyMaxWarmIdleTimeout)
		}
	}
	return Policy{
		Name: name, Repository: realRepo, Remote: raw.Remote, Branch: raw.Branch,
		Companions:     companions,
		Targets:        ladder,
		OmitEnv:        raw.ProjectEnv != nil && !*raw.ProjectEnv,
		OmitMCP:        raw.ProjectMCP != nil && !*raw.ProjectMCP,
		MaxTurns:       raw.MaxTurns,
		MaxQueuedTurns: raw.MaxQueuedTurns, MaxQueuedBytes: raw.MaxQueuedBytes,
		TurnTimeout: timeout, WarmIdleTimeout: warmIdleTimeout,
		MaxPatchBytes: raw.MaxPatchBytes,
	}, nil
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
	Policy      string                    `json:"policy"`
	Task        string                    `json:"task"`
	PullRequest *RemotePullRequestBinding `json:"pull_request,omitempty"`
}

// RemotePullRequestBinding selects one GitHub pull-request head through the
// operator-owned remote configured by the session policy. The caller cannot
// name a repository, remote, or arbitrary ref, and the expected head makes a
// PR update racing session creation fail closed instead of silently changing
// the task's approved source.
type RemotePullRequestBinding struct {
	Number     int    `json:"number"`
	HeadCommit string `json:"head_commit"`
}

func cloneSessionPullRequestBinding(value *session.PullRequestBinding) *session.PullRequestBinding {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

type Runner interface {
	Run(context.Context, session.Session, session.Turn) (session.Turn, error)
}

type sessionRunnerStartupCleaner interface {
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

type sessionRunnerCloser interface {
	CloseWarmSessions() error
}

type sessionRunnerStartupReaper interface {
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
	Logger              *slog.Logger
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

type Service struct {
	store               *session.Store
	stateRoot           string
	policies            map[string]Policy
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
	wg             sync.WaitGroup

	operationMu         sync.Mutex
	operationLocks      map[string]*sessionOperationLock
	createActive        map[string]bool
	createSlots         chan struct{}
	testBeforeCreatePin func() error
	testAfterTurnLease  func(session.Turn)
	runtimeMu           sync.Mutex
	runtimeLocks        map[string]*sessionOperationLock
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
		sourceCfg = config.Load()
	}
	policies := cfg.Policies
	if len(policies) == 0 {
		var err error
		policies, err = LoadPolicies(cfg.PolicyPath, sourceCfg)
		if err != nil {
			return nil, err
		}
	}
	store, err := session.Open(cfg.StateRoot)
	if err != nil {
		return nil, err
	}
	service := &Service{
		store: store, stateRoot: cfg.StateRoot,
		policies: cloneSessionPolicies(policies), sourceCfg: sourceCfg,
		rt: cfg.Runtime, executable: cfg.Executable, host: cfg.Host, runner: cfg.Runner,
		reviewGate:          cfg.ReviewGate,
		stopTimeout:         cfg.StopTimeout,
		cleanupInterval:     cfg.CleanupInterval,
		operationStaleAfter: cfg.OperationStaleAfter,
		log:                 cfg.Logger,
		workers:             make(map[string]*sessionWorker), active: make(map[string]*activeSessionTurn),
		pendingCancels: make(map[string]*pendingSessionCancel),
		operationLocks: make(map[string]*sessionOperationLock),
		createActive:   make(map[string]bool), createSlots: make(chan struct{}, sessionCreateConcurrency),
		runtimeLocks: make(map[string]*sessionOperationLock),
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

func resolvedSessionPolicyDigest(policy Policy) string {
	canonical := struct {
		Name            string            `json:"name"`
		Repository      string            `json:"repository"`
		Remote          string            `json:"remote,omitempty"`
		Branch          string            `json:"branch,omitempty"`
		Companions      []CompanionPolicy `json:"companions,omitempty"`
		Target          string            `json:"target"`
		OmitEnv         bool              `json:"omit_env,omitempty"`
		OmitMCP         bool              `json:"omit_mcp,omitempty"`
		MaxTurns        int               `json:"max_turns"`
		MaxQueuedTurns  int               `json:"max_queued_turns"`
		MaxQueuedBytes  int               `json:"max_queued_bytes"`
		TurnTimeout     int64             `json:"turn_timeout_ns"`
		WarmIdleTimeout int64             `json:"warm_idle_timeout_ns,omitempty"`
		MaxPatchBytes   int               `json:"max_patch_bytes"`
	}{
		Name: policy.Name, Repository: policy.Repository,
		Remote: policy.Remote, Branch: policy.Branch, Companions: policy.Companions,
		Target:  sessionTargetList(policy.Targets),
		OmitEnv: policy.OmitEnv, OmitMCP: policy.OmitMCP,
		MaxTurns: policy.MaxTurns, MaxQueuedTurns: policy.MaxQueuedTurns,
		MaxQueuedBytes: policy.MaxQueuedBytes, TurnTimeout: int64(policy.TurnTimeout),
		WarmIdleTimeout: int64(policy.WarmIdleTimeout),
		MaxPatchBytes:   policy.MaxPatchBytes,
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
	s.runtimeMu.Lock()
	lock := s.runtimeLocks[sessionID]
	if lock == nil {
		lock = &sessionOperationLock{}
		s.runtimeLocks[sessionID] = lock
	}
	lock.refs++
	s.runtimeMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.runtimeMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.runtimeLocks, sessionID)
		}
		s.runtimeMu.Unlock()
	}
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
	interrupted, err := s.store.ListInterruptedTurns(parent)
	if err != nil {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return err
	}
	reaper, canReap := s.runner.(sessionRunnerStartupReaper)
	byID := make(map[string]session.Session, len(sessions))
	for _, sess := range sessions {
		byID[sess.ID] = sess
	}
	for _, turn := range interrupted {
		sess, ok := byID[turn.SessionID]
		if !ok {
			s.mu.Lock()
			s.starting = false
			s.mu.Unlock()
			return fmt.Errorf("startup recovery session %s is missing for turn %s", turn.SessionID, turn.ID)
		}
		if !canReap {
			s.mu.Lock()
			s.starting = false
			s.mu.Unlock()
			return errors.New("startup recovery cannot prove interrupted runtime cleanup")
		}
		if err := reaper.ReapInterruptedTurn(parent, sess, turn); err != nil {
			s.mu.Lock()
			s.starting = false
			s.mu.Unlock()
			return fmt.Errorf("reap interrupted session turn %s: %w", turn.ID, err)
		}
	}
	if cleaner, ok := s.runner.(sessionRunnerStartupCleaner); ok {
		for _, sess := range sessions {
			if err := cleaner.CleanupSession(parent, sess); err != nil {
				s.host.warnf("could not clean historical session runtime state %s: %v", sess.ID, err)
			}
		}
	}
	if _, err := s.store.ReconcileInterruptedTurns(parent); err != nil {
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
		if sess.QueuedTurnCount > 0 {
			s.ensureWorkerLocked(sess.ID)
		}
	}
	for _, worker := range s.workers {
		s.triggerWorker(worker)
	}
	s.mu.Unlock()
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
			s.cleanupParkedSessions(ctx)
			if err := s.reconcileInterruptedOperations(ctx, false); err != nil && ctx.Err() == nil {
				s.log.Error("session operation reconciliation failed", "error", err)
			}
		}
	}
}

func (s *Service) cleanupParkedSessions(ctx context.Context) {
	parkedCleaner, parkedOK := s.runner.(sessionRunnerParkedCleaner)
	cleaner, cleanupOK := s.runner.(sessionRunnerStartupCleaner)
	if !parkedOK && !cleanupOK {
		return
	}
	sessions, err := s.store.ListSessionsForRecovery(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.host.warnf("could not list parked sessions for runtime cleanup: %v", err)
		}
		return
	}
	for _, candidate := range sessions {
		if candidate.State == session.SessionDiscarded ||
			candidate.Activity != session.ActivityParked || candidate.ActiveTurnID != "" {
			continue
		}
		unlock := s.lockSessionRuntime(candidate.ID)
		current, getErr := s.store.GetSession(ctx, candidate.ID)
		if getErr == nil && current.Activity == session.ActivityParked && current.ActiveTurnID == "" {
			if parkedOK {
				getErr = parkedCleaner.CleanupParkedSession(ctx, current)
			} else {
				getErr = cleaner.CleanupSession(ctx, current)
			}
		}
		unlock()
		if getErr != nil && ctx.Err() == nil {
			s.host.warnf("could not clean parked session runtime state %s: %v", candidate.ID, getErr)
		}
	}
}

func (s *Service) runBoundSessionTurn(ctx context.Context, bound session.Session, leased session.Turn) (session.Turn, error) {
	unlock := s.lockSessionRuntime(bound.ID)
	defer unlock()
	return s.runner.Run(s.sessionTurnContext(ctx, bound), bound, leased)
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
	if req.PullRequest != nil && (req.PullRequest.Number < 1 ||
		!validSessionWorkspaceCommit(req.PullRequest.HeadCommit)) {
		return session.Operation{}, &session.Error{
			Code: session.CodeInvalidRequest, Detail: "pull request number and exact head commit are required",
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
	OperationID     string                        `json:"operation_id"`
	Policy          Policy                        `json:"policy"`
	Task            string                        `json:"task"`
	SessionID       string                        `json:"session_id"`
	ForkName        string                        `json:"fork_name"`
	BaseCommit      string                        `json:"base_commit"`
	WorkspaceCommit string                        `json:"workspace_commit"`
	PullRequest     *session.PullRequestBinding   `json:"pull_request,omitempty"`
	Companions      []session.CompanionRepository `json:"companions,omitempty"`
}

func (s *Service) captureCreateIntent(op session.Operation, req CreateRemoteSessionRequest) (sessionCreateIntent, error) {
	policy, err := s.policy(req.Policy)
	if err != nil {
		return sessionCreateIntent{}, err
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
	}
	if req.PullRequest != nil {
		intent.PullRequest = &session.PullRequestBinding{
			Number:     req.PullRequest.Number,
			Ref:        fmt.Sprintf("refs/pull/%d/head", req.PullRequest.Number),
			HeadCommit: req.PullRequest.HeadCommit,
		}
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
	if intent.OperationID != op.ID || intent.SessionID == "" ||
		!forkspace.ValidName(intent.ForkName) {
		return s.rejectCreateIntent(ctx, op.ID, "create operation intent is invalid")
	}
	if !validSessionIntentTargets(intent.Policy.Targets) {
		return s.rejectCreateIntent(ctx, op.ID, "create operation intent has no valid target")
	}
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
	workspace, err := ensureSessionWorkspaceContext(ctx, intent.Policy.Repository, intent.ForkName, workspaceCommit)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return session.Session{}, err
		}
		return session.Session{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("ensure session workspace: %w", err))
	}
	companions := make([]session.CompanionRepository, 0, len(intent.Companions))
	failCreate := func(cause error) (session.Session, error) {
		if ctx.Err() != nil && errors.Is(cause, ctx.Err()) {
			// Deterministic intent and workspace names make restart recovery the
			// safe cleanup owner. Contextless rollback here would outlive Stop and
			// race that recovery process.
			return session.Session{}, cause
		}
		if cleanupErr := rollbackSessionCreate(workspace, companions); cleanupErr != nil {
			cause = errors.Join(
				cause,
				fmt.Errorf("rollback partial session creation: %w", cleanupErr),
			)
		}
		return session.Session{}, s.failServiceOperation(ctx, op.ID, cause)
	}
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
	createReq := session.CreateSessionRequest{
		// A session starts on the ladder's first rung; a rate limit rotates it to the next.
		ID: intent.SessionID, ExternalRef: intent.Task, Target: intent.Policy.Targets[0].String(), Policy: intent.Policy.Name,
		PolicyDigest: resolvedSessionPolicyDigest(intent.Policy),
		OmitEnv:      intent.Policy.OmitEnv,
		OmitMCP:      intent.Policy.OmitMCP,
		Repository:   intent.Policy.Repository, Workspace: workspace.Path, ForkName: intent.ForkName,
		BaseCommit: intent.BaseCommit, PullRequest: intent.PullRequest, Companions: companions,
		MaxTurns:       intent.Policy.MaxTurns,
		MaxQueuedTurns: intent.Policy.MaxQueuedTurns, MaxQueuedBytes: intent.Policy.MaxQueuedBytes,
		TurnTimeout: intent.Policy.TurnTimeout, MaxPatchBytes: intent.Policy.MaxPatchBytes,
	}
	sess, err := s.store.CreateSession(ctx, "create-session-"+op.ID, createReq)
	if err != nil {
		return failCreate(err)
	}
	result, err := json.Marshal(sess)
	if err != nil {
		return session.Session{}, err
	}
	if err := s.store.CompleteOperation(ctx, op.ID, "session", sess.ID, result); err != nil {
		return session.Session{}, err
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
	var source *RemotePullRequestBinding
	if intent.PullRequest != nil {
		if intent.PullRequest.Ref != fmt.Sprintf("refs/pull/%d/head", intent.PullRequest.Number) {
			return sessionCreateIntent{}, errors.New("create operation pull request ref is invalid")
		}
		source = &RemotePullRequestBinding{
			Number: intent.PullRequest.Number, HeadCommit: intent.PullRequest.HeadCommit,
		}
	}
	pins, err := pinSessionPolicyRepositories(ctx, intent.Policy, source)
	if err != nil {
		return sessionCreateIntent{}, err
	}
	intent.BaseCommit = pins.creationBase
	intent.WorkspaceCommit = pins.workspaceHead
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
	return s.store.GetSession(ctx, id)
}

func (s *Service) ListSessions(ctx context.Context, limit int) ([]session.Session, error) {
	return s.store.ListSessions(ctx, limit)
}

func (s *Service) SubmitTurn(ctx context.Context, key string, req session.SubmitTurnRequest) (session.Turn, error) {
	if err := s.validateTurnEscalation(ctx, req); err != nil {
		return session.Turn{}, err
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
	if req.MinTargetIndex <= 0 {
		return nil
	}
	bound, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		// Not this check's failure to report: the store admits the turn against its own
		// canonical errors and records the operation a retry reads back.
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

func (s *Service) GetTurn(ctx context.Context, sessionID, turnID string) (session.Turn, error) {
	return s.store.GetTurn(ctx, sessionID, turnID)
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
	} else if cleaner, ok := s.runner.(sessionRunnerStartupCleaner); ok {
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
	if sess.PullRequest != nil {
		changes.PullRequestTree, err = sessionWorkspaceTree(
			sess.Workspace, sess.PullRequest.HeadCommit,
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
	if sess.PullRequest != nil {
		changes.PullRequestTree, err = sessionWorkspaceTree(
			sess.Workspace, sess.PullRequest.HeadCommit,
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
	if sess.Revision != req.ExpectedRevision || sess.State != session.SessionClosed || sess.ActiveTurnID != "" || sess.QueuedTurnCount != 0 {
		return PlanDiscardResult{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeInvalidSessionState, Detail: "discard planning requires a closed idle session"})
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
		companionPlan, err := planSessionCompanionDiscard(companion)
		if err != nil {
			return PlanDiscardResult{}, s.failServiceOperation(
				ctx, op.ID,
				fmt.Errorf("plan companion %q discard: %w", companion.Name, err),
			)
		}
		companions = append(companions, companionPlan)
	}
	result := PlanDiscardResult{
		OperationID: op.ID,
		Plan: DiscardPlan{
			SessionID: sess.ID, Revision: sess.Revision,
			Workspace: plan, Companions: companions,
		},
	}
	data, err := json.Marshal(result)
	if err != nil {
		return PlanDiscardResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := s.store.MarkOperationRunning(ctx, op.ID, data); err != nil {
		return PlanDiscardResult{}, err
	}
	if err := s.store.CompleteOperation(ctx, op.ID, "discard_plan", sess.ID, data); err != nil {
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

func (s *Service) executeDiscardRequest(ctx context.Context, op session.Operation, req DiscardRequest) (session.Session, error) {
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
	if op.State == session.OperationReserved {
		if err := s.store.MarkOperationRunning(ctx, op.ID, intentData); err != nil {
			return session.Session{}, err
		}
	}
	sess, err := s.store.GetSession(ctx, planned.Plan.SessionID)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
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
	if s.rt.Name != "" {
		if err := box.DownServices(
			s.rt,
			planned.Plan.Workspace.Workspace,
			planned.Plan.Workspace.Repo,
			true,
			io.Discard,
			io.Discard,
		); err != nil {
			return session.Session{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("remove session services: %w", err))
		}
	}
	if err := discardSessionWorkspace(planned.Plan.Workspace); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeDiscardPlanStale, Detail: boundedSessionServiceError(err)})
	}
	for _, companion := range planned.Plan.Companions {
		if err := discardSessionCompanion(companion); err != nil {
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
	if turn.State == session.TurnQueued {
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
	data, err := json.Marshal(turn)
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
	var turn session.Turn
	if err := json.Unmarshal(op.Result, &turn); err != nil {
		return session.Turn{}, wrapServiceOperationError(op.ID,
			fmt.Errorf("decode cancel operation result: %w", err))
	}
	if turn.ID == "" {
		return session.Turn{}, wrapServiceOperationError(op.ID,
			errors.New("decode cancel operation result: missing turn id"))
	}
	return turn, nil
}

func (s *Service) GetOperation(ctx context.Context, key string) (session.Operation, error) {
	return s.store.GetOperation(ctx, key)
}

func (s *Service) GetOperationByID(ctx context.Context, id string) (session.Operation, error) {
	return s.store.GetOperationByID(ctx, id)
}
