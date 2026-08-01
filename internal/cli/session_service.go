package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/ui"
	"gopkg.in/yaml.v3"
)

const (
	sessionPolicyVersion             = 1
	sessionPolicyFileLimit           = 1 << 20
	sessionPolicyMaxCompanions       = 32
	sessionPolicyMaxTurnTimeout      = 24 * time.Hour
	sessionPolicyMaxWarmIdleTimeout  = time.Hour
	sessionServiceDefaultStopTimeout = 5 * time.Second
	sessionServiceCleanupInterval    = time.Minute
)

// SessionPolicy is operator-owned authority for one remote session. It is intentionally small:
// repository, target, and resource bounds are not request fields.
type SessionPolicy struct {
	Name            string
	Repository      string
	Companions      []SessionCompanionPolicy
	Target          string
	MaxTurns        int
	MaxQueuedTurns  int
	MaxQueuedBytes  int
	TurnTimeout     time.Duration
	WarmIdleTimeout time.Duration
	MaxPatchBytes   int
}

type SessionCompanionPolicy struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
}

type rawSessionPolicyFile struct {
	Version  int                         `yaml:"version"`
	Policies map[string]rawSessionPolicy `yaml:"policies"`
}

type rawSessionPolicy struct {
	Repository      string                      `yaml:"repository"`
	Companions      []rawSessionCompanionPolicy `yaml:"companions"`
	Target          string                      `yaml:"target"`
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
}

// LoadSessionPolicies parses the strict operator policy file. A config is required when the
// caller wants credential availability checked; passing nil performs syntax/target/repository
// validation only and is useful for isolated parser tests.
func LoadSessionPolicies(path string, cfg *config.Config) (map[string]SessionPolicy, error) {
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
	data, err := io.ReadAll(io.LimitReader(file, sessionPolicyFileLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read session policy file: %w", err)
	}
	if len(data) > sessionPolicyFileLimit {
		return nil, fmt.Errorf("session policy file exceeds %d bytes", sessionPolicyFileLimit)
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
		if info.Mode().Perm()&0o022 != 0 {
			return errors.New("session policy ancestry is group/world writable")
		}
		if owner, ok := sessionFileOwner(info); !ok {
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

func parseSessionPolicies(data []byte, cfg *config.Config) (map[string]SessionPolicy, error) {
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
	policies := make(map[string]SessionPolicy, len(raw.Policies))
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

func validateSessionPolicy(name string, raw rawSessionPolicy, cfg *config.Config) (SessionPolicy, error) {
	if raw.Repository == "" || !filepath.IsAbs(raw.Repository) || filepath.Clean(raw.Repository) != raw.Repository {
		return SessionPolicy{}, errors.New("repository must be an absolute, clean path")
	}
	realRepo, err := realGitRepository(raw.Repository)
	if err != nil {
		return SessionPolicy{}, err
	}
	if len(raw.Companions) > sessionPolicyMaxCompanions {
		return SessionPolicy{}, fmt.Errorf(
			"companions are limited to %d repositories",
			sessionPolicyMaxCompanions,
		)
	}
	companions := make([]SessionCompanionPolicy, 0, len(raw.Companions))
	seenNames := make(map[string]bool, len(raw.Companions))
	seenRepositories := map[string]bool{realRepo: true}
	for _, companion := range raw.Companions {
		if !validCompanionRepositoryName(companion.Name) {
			return SessionPolicy{}, fmt.Errorf(
				"companion name %q must use 1-48 lowercase letters, numbers, hyphens, or underscores and cannot be primary",
				companion.Name,
			)
		}
		if seenNames[companion.Name] {
			return SessionPolicy{}, fmt.Errorf("companion name %q is duplicated", companion.Name)
		}
		if companion.Repository == "" || !filepath.IsAbs(companion.Repository) ||
			filepath.Clean(companion.Repository) != companion.Repository {
			return SessionPolicy{}, fmt.Errorf(
				"companion %q repository must be an absolute, clean path",
				companion.Name,
			)
		}
		realCompanion, err := realGitRepository(companion.Repository)
		if err != nil {
			return SessionPolicy{}, fmt.Errorf("companion %q: %w", companion.Name, err)
		}
		if seenRepositories[realCompanion] {
			return SessionPolicy{}, fmt.Errorf(
				"companion %q repeats the primary or another companion repository",
				companion.Name,
			)
		}
		seenNames[companion.Name] = true
		seenRepositories[realCompanion] = true
		companions = append(companions, SessionCompanionPolicy{
			Name: companion.Name, Repository: realCompanion,
		})
	}
	target, err := agents.ParseTarget(raw.Target)
	if err != nil {
		return SessionPolicy{}, fmt.Errorf("invalid ACP target: %w", err)
	}
	if len(target.Accounts) > 1 {
		return SessionPolicy{}, errors.New("target must name zero or one account")
	}
	agent, ok := agents.Get(target.Provider)
	checkCfg := cfg
	if checkCfg == nil {
		checkCfg = &config.Config{}
	}
	if !ok || len(agent.ACP(checkCfg)) == 0 {
		return SessionPolicy{}, errors.New("target provider has no ACP adapter")
	}
	account := target.Account()
	if cfg != nil {
		if account == "" {
			account = cfg.DefaultProfileOf(target.Provider)
		}
		if !box.ProfileAuthed(cfg, target.Provider, account) {
			return SessionPolicy{}, fmt.Errorf("target account %q is not authenticated", account)
		}
		target.Accounts = []string{account}
	}
	if raw.MaxTurns <= 0 || raw.MaxTurns > session.MaxTurnsLimit {
		return SessionPolicy{}, fmt.Errorf("max_turns must be between 1 and %d", session.MaxTurnsLimit)
	}
	if raw.MaxQueuedTurns <= 0 || raw.MaxQueuedTurns > session.MaxQueuedTurnsLimit {
		return SessionPolicy{}, fmt.Errorf("max_queued_turns must be between 1 and %d", session.MaxQueuedTurnsLimit)
	}
	if raw.MaxQueuedBytes <= 0 || raw.MaxQueuedBytes > session.MaxQueuedBytesLimit {
		return SessionPolicy{}, fmt.Errorf("max_queued_bytes must be between 1 and %d", session.MaxQueuedBytesLimit)
	}
	if raw.MaxPatchBytes <= 0 || raw.MaxPatchBytes > sessionWorkspacePatchLimit {
		return SessionPolicy{}, fmt.Errorf("max_patch_bytes must be between 1 and %d", sessionWorkspacePatchLimit)
	}
	if raw.TurnTimeout == "" {
		return SessionPolicy{}, errors.New("turn_timeout is required")
	}
	timeout, err := time.ParseDuration(raw.TurnTimeout)
	if err != nil || timeout <= 0 || timeout > sessionPolicyMaxTurnTimeout {
		return SessionPolicy{}, fmt.Errorf("turn_timeout must be positive and no longer than %s", sessionPolicyMaxTurnTimeout)
	}
	var warmIdleTimeout time.Duration
	if raw.WarmIdleTimeout != "" {
		warmIdleTimeout, err = time.ParseDuration(raw.WarmIdleTimeout)
		if err != nil || warmIdleTimeout <= 0 || warmIdleTimeout > sessionPolicyMaxWarmIdleTimeout {
			return SessionPolicy{}, fmt.Errorf("warm_idle_timeout must be positive and no longer than %s", sessionPolicyMaxWarmIdleTimeout)
		}
	}
	return SessionPolicy{
		Name: name, Repository: realRepo, Companions: companions,
		Target: target.String(), MaxTurns: raw.MaxTurns,
		MaxQueuedTurns: raw.MaxQueuedTurns, MaxQueuedBytes: raw.MaxQueuedBytes,
		TurnTimeout: timeout, WarmIdleTimeout: warmIdleTimeout,
		MaxPatchBytes: raw.MaxPatchBytes,
	}, nil
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
}

type SessionRunner interface {
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

type SessionRunnerFunc func(context.Context, session.Session, session.Turn) (session.Turn, error)

func (f SessionRunnerFunc) Run(ctx context.Context, sess session.Session, turn session.Turn) (session.Turn, error) {
	return f(ctx, sess, turn)
}

type SessionRunnerFactory func(*session.Store) SessionRunner

type SessionServiceConfig struct {
	StateRoot       string
	PolicyPath      string
	Policies        map[string]SessionPolicy
	SourceConfig    *config.Config
	Config          *config.Config
	Runtime         runtime.Runtime
	Executable      string
	Runner          SessionRunner
	RunnerFactory   SessionRunnerFactory
	ReviewGate      SessionReviewGate
	StopTimeout     time.Duration
	CleanupInterval time.Duration
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

type sessionOperationLock struct {
	mu   sync.Mutex
	refs int
}

type SessionService struct {
	store           *session.Store
	stateRoot       string
	policies        map[string]SessionPolicy
	sourceCfg       *config.Config
	rt              runtime.Runtime
	executable      string
	runner          SessionRunner
	reviewGate      SessionReviewGate
	stopTimeout     time.Duration
	cleanupInterval time.Duration

	stopMu   sync.Mutex
	mu       sync.Mutex
	started  bool
	starting bool
	ctx      context.Context
	cancel   context.CancelFunc
	workers  map[string]*sessionWorker
	active   map[string]*activeSessionTurn
	wg       sync.WaitGroup

	operationMu    sync.Mutex
	operationLocks map[string]*sessionOperationLock
	runtimeMu      sync.Mutex
	runtimeLocks   map[string]*sessionOperationLock
}

func NewSessionService(cfg SessionServiceConfig) (*SessionService, error) {
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
		policies, err = LoadSessionPolicies(cfg.PolicyPath, sourceCfg)
		if err != nil {
			return nil, err
		}
	}
	store, err := session.Open(cfg.StateRoot)
	if err != nil {
		return nil, err
	}
	service := &SessionService{
		store: store, stateRoot: cfg.StateRoot,
		policies: cloneSessionPolicies(policies), sourceCfg: sourceCfg,
		rt: cfg.Runtime, executable: cfg.Executable, runner: cfg.Runner,
		reviewGate:      cfg.ReviewGate,
		stopTimeout:     cfg.StopTimeout,
		cleanupInterval: cfg.CleanupInterval,
		workers:         make(map[string]*sessionWorker), active: make(map[string]*activeSessionTurn),
		operationLocks: make(map[string]*sessionOperationLock),
		runtimeLocks:   make(map[string]*sessionOperationLock),
	}
	if service.stopTimeout <= 0 {
		service.stopTimeout = sessionServiceDefaultStopTimeout
	}
	if service.cleanupInterval <= 0 {
		service.cleanupInterval = sessionServiceCleanupInterval
	}
	if cfg.RunnerFactory != nil {
		service.runner = cfg.RunnerFactory(store)
	}
	return service, nil
}

func cloneSessionPolicies(in map[string]SessionPolicy) map[string]SessionPolicy {
	out := make(map[string]SessionPolicy, len(in))
	for name, policy := range in {
		if policy.Name == "" {
			policy.Name = name
		}
		policy.Companions = append([]SessionCompanionPolicy(nil), policy.Companions...)
		out[name] = policy
	}
	return out
}

func resolvedSessionPolicyDigest(policy SessionPolicy) string {
	canonical := struct {
		Name            string                   `json:"name"`
		Repository      string                   `json:"repository"`
		Companions      []SessionCompanionPolicy `json:"companions,omitempty"`
		Target          string                   `json:"target"`
		MaxTurns        int                      `json:"max_turns"`
		MaxQueuedTurns  int                      `json:"max_queued_turns"`
		MaxQueuedBytes  int                      `json:"max_queued_bytes"`
		TurnTimeout     int64                    `json:"turn_timeout_ns"`
		WarmIdleTimeout int64                    `json:"warm_idle_timeout_ns,omitempty"`
		MaxPatchBytes   int                      `json:"max_patch_bytes"`
	}{
		Name: policy.Name, Repository: policy.Repository, Companions: policy.Companions,
		Target:   policy.Target,
		MaxTurns: policy.MaxTurns, MaxQueuedTurns: policy.MaxQueuedTurns,
		MaxQueuedBytes: policy.MaxQueuedBytes, TurnTimeout: int64(policy.TurnTimeout),
		WarmIdleTimeout: int64(policy.WarmIdleTimeout),
		MaxPatchBytes:   policy.MaxPatchBytes,
	}
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *SessionService) Store() *session.Store { return s.store }

func (s *SessionService) lockOperation(key string) func() {
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

func (s *SessionService) lockSessionRuntime(sessionID string) func() {
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

func (s *SessionService) Start(parent context.Context) error {
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
				ui.Warn("could not clean historical session runtime state %s: %v", sess.ID, err)
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
	go s.runParkedSessionCleanup(ctx)
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

func (s *SessionService) runParkedSessionCleanup(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanupParkedSessions(ctx)
		}
	}
}

func (s *SessionService) cleanupParkedSessions(ctx context.Context) {
	parkedCleaner, parkedOK := s.runner.(sessionRunnerParkedCleaner)
	cleaner, cleanupOK := s.runner.(sessionRunnerStartupCleaner)
	if !parkedOK && !cleanupOK {
		return
	}
	sessions, err := s.store.ListSessionsForRecovery(ctx)
	if err != nil {
		if ctx.Err() == nil {
			ui.Warn("could not list parked sessions for runtime cleanup: %v", err)
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
			ui.Warn("could not clean parked session runtime state %s: %v", candidate.ID, getErr)
		}
	}
}

func (s *SessionService) runBoundSessionTurn(ctx context.Context, bound session.Session, leased session.Turn) (session.Turn, error) {
	unlock := s.lockSessionRuntime(bound.ID)
	defer unlock()
	if policy, ok := s.policies[bound.Policy]; ok &&
		resolvedSessionPolicyDigest(policy) == bound.PolicyDigest && policy.WarmIdleTimeout > 0 {
		ctx = context.WithValue(ctx, sessionWarmIdleTimeoutContextKey{}, policy.WarmIdleTimeout)
	}
	return s.runner.Run(ctx, bound, leased)
}

// ensureRunner keeps host-local construction (and runtime detection) out of service creation.
// Opening the service first lets callers acquire the durable state-root lock before doing any
// runner-specific startup work, and lets pure local commands remain usable without a runtime.
func (s *SessionService) ensureRunner() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runner != nil {
		if s.reviewGate == nil {
			s.reviewGate = defaultSessionReviewGate{cfg: s.sourceCfg, rt: s.rt}
		}
		return nil
	}
	if s.rt.Name == "" {
		rt, err := runtime.Detect(s.sourceCfg.RuntimeName)
		if err != nil {
			return err
		}
		s.rt = rt
	}
	s.runner = newSessionTurnRunner(s.sourceCfg, s.store.Root(), s.store, s.rt, s.executable)
	if s.reviewGate == nil {
		s.reviewGate = defaultSessionReviewGate{cfg: s.sourceCfg, rt: s.rt}
	}
	return nil
}

func (s *SessionService) Stop() error {
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
	s.wg.Wait()
	s.mu.Lock()
	s.workers = make(map[string]*sessionWorker)
	s.active = make(map[string]*activeSessionTurn)
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

func (s *SessionService) ensureWorkerLocked(sessionID string) *sessionWorker {
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

func (s *SessionService) triggerWorker(worker *sessionWorker) {
	if worker == nil {
		return
	}
	select {
	case worker.trigger <- struct{}{}:
	default:
	}
}

func (s *SessionService) schedule(sessionID string) {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	worker := s.ensureWorkerLocked(sessionID)
	s.triggerWorker(worker)
	s.mu.Unlock()
}

func (s *SessionService) runSessionWorker(ctx context.Context, worker *sessionWorker) {
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

func (s *SessionService) removeWorker(worker *sessionWorker) {
	if worker == nil {
		return
	}
	s.mu.Lock()
	if s.workers[worker.sessionID] == worker {
		delete(s.workers, worker.sessionID)
	}
	s.mu.Unlock()
}

func (s *SessionService) drainSession(ctx context.Context, sessionID string) {
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
		turnCtx, turnCancel := context.WithTimeout(ctx, bound.TurnTimeout)
		active := &activeSessionTurn{cancel: turnCancel, done: make(chan struct{})}
		s.mu.Lock()
		s.active[leased.ID] = active
		s.mu.Unlock()
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
	if len(detail) > session.MaxErrorDetailBytes {
		detail = detail[:session.MaxErrorDetailBytes]
	}
	return detail
}

func (s *SessionService) policy(name string) (SessionPolicy, error) {
	if name == "" {
		return SessionPolicy{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "policy name is required"}
	}
	policy, ok := s.policies[name]
	if !ok {
		return SessionPolicy{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "unknown execution policy"}
	}
	return policy, nil
}

func (s *SessionService) CreateRemoteSession(ctx context.Context, key string, req CreateRemoteSessionRequest) (session.Session, error) {
	unlock := s.lockOperation(key)
	defer unlock()

	if req.Policy == "" || len(req.Policy) > session.MaxIDBytes || !utf8SessionText(req.Policy) || req.Task == "" || len(req.Task) > session.MaxExternalRefBytes || !utf8SessionText(req.Task) {
		return session.Session{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "policy and bounded task are required"}
	}
	op, replay, err := s.store.ReserveOperation(ctx, "CreateRemoteSession", key, req)
	if err != nil {
		return session.Session{}, err
	}
	if replay {
		if op.State == session.OperationReserved {
			return s.executeCreate(ctx, op, req)
		}
		return s.replayCreateOperation(ctx, op)
	}
	return s.executeCreate(ctx, op, req)
}

func (s *SessionService) replayCreateOperation(ctx context.Context, op session.Operation) (session.Session, error) {
	switch op.State {
	case session.OperationSucceeded:
		var sess session.Session
		if err := json.Unmarshal(op.Result, &sess); err != nil {
			return session.Session{}, fmt.Errorf("decode create operation result: %w", err)
		}
		if sess.ID == "" {
			return session.Session{}, errors.New("decode create operation result: missing session id")
		}
		return sess, nil
	case session.OperationFailed:
		return session.Session{}, &session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail}
	case session.OperationRunning:
		var intent sessionCreateIntent
		if err := json.Unmarshal(op.Result, &intent); err != nil {
			return session.Session{}, &session.Error{Code: session.CodeOperationUncertain, Detail: "create operation intent is unreadable"}
		}
		return s.executeCreateIntent(ctx, op, intent)
	default:
		return session.Session{}, session.ErrOperationUncertain
	}
}

type sessionCreateIntent struct {
	OperationID string                        `json:"operation_id"`
	Policy      SessionPolicy                 `json:"policy"`
	Task        string                        `json:"task"`
	SessionID   string                        `json:"session_id"`
	ForkName    string                        `json:"fork_name"`
	BaseCommit  string                        `json:"base_commit"`
	Companions  []session.CompanionRepository `json:"companions,omitempty"`
}

func (s *SessionService) executeCreate(ctx context.Context, op session.Operation, req CreateRemoteSessionRequest) (session.Session, error) {
	policy, err := s.policy(req.Policy)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	base, err := sessionWorkspaceCommit(policy.Repository, "HEAD")
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("pin parent HEAD: %w", err))
	}
	sessionID := deterministicSessionID(op.ID)
	companions := make([]session.CompanionRepository, 0, len(policy.Companions))
	for _, companion := range policy.Companions {
		commit, err := sessionWorkspaceCommit(companion.Repository, "HEAD")
		if err != nil {
			return session.Session{}, s.failServiceOperation(
				ctx, op.ID,
				fmt.Errorf("pin companion %q HEAD: %w", companion.Name, err),
			)
		}
		workspace, err := sessionCompanionWorkspace(
			s.store.Root(), sessionID, companion.Name,
		)
		if err != nil {
			return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
		}
		companions = append(companions, session.CompanionRepository{
			Name: companion.Name, Repository: companion.Repository,
			Workspace: workspace, BaseCommit: commit,
		})
	}
	intent := sessionCreateIntent{
		OperationID: op.ID, Policy: policy, Task: req.Task,
		SessionID: sessionID, ForkName: deterministicForkName(op.ID), BaseCommit: base,
		Companions: companions,
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := s.store.MarkOperationRunning(ctx, op.ID, data); err != nil {
		return session.Session{}, err
	}
	return s.executeCreateIntent(ctx, op, intent)
}

func deterministicSessionID(operationID string) string {
	sum := sha256.Sum256([]byte(operationID))
	return "remote_" + hex.EncodeToString(sum[:16])
}

func deterministicForkName(operationID string) string {
	sum := sha256.Sum256([]byte("fork\x00" + operationID))
	return "remote-" + hex.EncodeToString(sum[:12])
}

func (s *SessionService) executeCreateIntent(ctx context.Context, op session.Operation, intent sessionCreateIntent) (session.Session, error) {
	if intent.OperationID != op.ID || intent.SessionID == "" ||
		!validForkName(intent.ForkName) ||
		!validSessionWorkspaceCommit(intent.BaseCommit) {
		return session.Session{}, &session.Error{Code: session.CodeOperationUncertain, Detail: "create operation intent is invalid"}
	}
	workspace, err := ensureSessionWorkspace(intent.Policy.Repository, intent.ForkName, intent.BaseCommit)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("ensure session workspace: %w", err))
	}
	companions := make([]session.CompanionRepository, 0, len(intent.Companions))
	failCreate := func(cause error) (session.Session, error) {
		if cleanupErr := rollbackSessionCreate(workspace, companions); cleanupErr != nil {
			cause = errors.Join(
				cause,
				fmt.Errorf("rollback partial session creation: %w", cleanupErr),
			)
		}
		return session.Session{}, s.failServiceOperation(ctx, op.ID, cause)
	}
	for _, companion := range intent.Companions {
		resolved, err := ensureSessionCompanion(
			s.store.Root(), intent.SessionID, companion,
		)
		if err != nil {
			return failCreate(
				fmt.Errorf("ensure companion %q: %w", companion.Name, err),
			)
		}
		companions = append(companions, resolved)
	}
	createReq := session.CreateSessionRequest{
		ID: intent.SessionID, ExternalRef: intent.Task, Target: intent.Policy.Target, Policy: intent.Policy.Name,
		PolicyDigest: resolvedSessionPolicyDigest(intent.Policy),
		Repository:   intent.Policy.Repository, Workspace: workspace.Path, ForkName: intent.ForkName,
		BaseCommit: intent.BaseCommit, Companions: companions,
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
	plan, err := planSessionWorkspaceDiscard(
		workspace.Repo, workspace.Path, false, false,
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

func (s *SessionService) failServiceOperation(ctx context.Context, id string, err error) error {
	code := session.CodeOf(err)
	if code == "" {
		code = session.CodeInternal
	}
	_ = s.store.FailOperation(ctx, id, code, boundedSessionServiceError(err))
	return err
}

func utf8SessionText(value string) bool {
	return !strings.ContainsRune(value, '\x00') && utf8.ValidString(value)
}

func (s *SessionService) GetSession(ctx context.Context, id string) (session.Session, error) {
	return s.store.GetSession(ctx, id)
}

func (s *SessionService) PrepareSession(ctx context.Context, id string, expectedRevision int64) (session.Session, error) {
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

func (s *SessionService) ListSessions(ctx context.Context, limit int) ([]session.Session, error) {
	return s.store.ListSessions(ctx, limit)
}

func (s *SessionService) SubmitTurn(ctx context.Context, key string, req session.SubmitTurnRequest) (session.Turn, error) {
	turn, err := s.store.SubmitTurn(ctx, key, req)
	if err == nil {
		s.schedule(req.SessionID)
	}
	return turn, err
}

func (s *SessionService) GetTurn(ctx context.Context, sessionID, turnID string) (session.Turn, error) {
	return s.store.GetTurn(ctx, sessionID, turnID)
}

func (s *SessionService) ListTurns(ctx context.Context, sessionID string, afterOrdinal int64, limit int) ([]session.Turn, error) {
	return s.store.ListTurns(ctx, sessionID, afterOrdinal, limit)
}

func (s *SessionService) GetOutputArtifact(ctx context.Context, sessionID, turnID, artifactID string) (session.OutputArtifact, error) {
	return s.store.GetOutputArtifact(ctx, sessionID, turnID, artifactID)
}

func (s *SessionService) ListEvents(ctx context.Context, sessionID string, after int64, limit int) ([]session.Event, error) {
	return s.store.ListEvents(ctx, sessionID, after, limit)
}

func (s *SessionService) ExtendBudget(ctx context.Context, key string, req session.ExtendBudgetRequest) (session.Session, error) {
	return s.store.ExtendBudget(ctx, key, req)
}

func (s *SessionService) Close(ctx context.Context, key string, req session.CloseSessionRequest) (session.Session, error) {
	unlock := s.lockSessionRuntime(req.SessionID)
	defer unlock()
	current, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return session.Session{}, err
	}
	if current.State == session.SessionClosed {
		return s.store.CloseSession(ctx, key, req)
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
	return s.store.CloseSession(ctx, key, req)
}

func (s *SessionService) CloseSession(ctx context.Context, key string, req session.CloseSessionRequest) (session.Session, error) {
	return s.Close(ctx, key, req)
}

func (s *SessionService) GetChanges(ctx context.Context, sessionID string) (SessionWorkspaceChanges, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return SessionWorkspaceChanges{}, err
	}
	return inspectSessionChanges(sess.Repository, sess.Workspace, sess.BaseCommit, sess.MaxPatchBytes)
}

func (s *SessionService) GetChangesPage(
	ctx context.Context,
	sessionID string,
	patchOffset int64,
	patchLimit int,
) (SessionWorkspaceChanges, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return SessionWorkspaceChanges{}, err
	}
	if patchLimit < 1 || patchLimit > sess.MaxPatchBytes {
		return SessionWorkspaceChanges{}, &session.Error{
			Code: session.CodeInvalidRequest,
			Detail: fmt.Sprintf(
				"patch limit must be between 1 and %d bytes",
				sess.MaxPatchBytes,
			),
		}
	}
	return inspectSessionChangesPage(
		sess.Repository,
		sess.Workspace,
		sess.BaseCommit,
		patchOffset,
		patchLimit,
	)
}

type PlanDiscardRequest struct {
	SessionID        string `json:"session_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	AcceptDirty      bool   `json:"accept_dirty"`
	AcceptUnmerged   bool   `json:"accept_unmerged"`
}

type SessionDiscardPlan struct {
	SessionID  string                        `json:"session_id"`
	Revision   int64                         `json:"revision"`
	Workspace  SessionWorkspaceDiscardPlan   `json:"workspace"`
	Companions []sessionCompanionDiscardPlan `json:"companions,omitempty"`
}

type PlanDiscardResult struct {
	OperationID string             `json:"operation_id"`
	Plan        SessionDiscardPlan `json:"plan"`
}

func (s *SessionService) PlanDiscard(ctx context.Context, key string, req PlanDiscardRequest) (PlanDiscardResult, error) {
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

func (s *SessionService) executePlanDiscard(ctx context.Context, op session.Operation, req PlanDiscardRequest) (PlanDiscardResult, error) {
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
	plan, err := planSessionWorkspaceDiscard(sess.Repository, sess.Workspace, req.AcceptDirty, req.AcceptUnmerged)
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
		Plan: SessionDiscardPlan{
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
		return PlanDiscardResult{}, &session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail}
	}
	if op.State != session.OperationSucceeded && op.State != session.OperationRunning {
		return PlanDiscardResult{}, session.ErrOperationUncertain
	}
	var result PlanDiscardResult
	if err := json.Unmarshal(op.Result, &result); err != nil {
		return PlanDiscardResult{}, err
	}
	return result, nil
}

type DiscardRequest struct {
	PlanOperationID string `json:"plan_operation_id"`
}

func (s *SessionService) Discard(ctx context.Context, key string, req DiscardRequest) (session.Session, error) {
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
				return session.Session{}, session.ErrOperationUncertain
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

func (s *SessionService) executeDiscardRequest(ctx context.Context, op session.Operation, req DiscardRequest) (session.Session, error) {
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

func (s *SessionService) executeDiscard(ctx context.Context, op session.Operation, planned PlanDiscardResult) (session.Session, error) {
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
			return session.Session{}, session.ErrOperationUncertain
		}
		completed, err := s.completeDiscardOperation(ctx, op.ID, sess)
		if err != nil {
			return session.Session{}, session.ErrOperationUncertain
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
			return session.Session{}, session.ErrOperationUncertain
		}
	}
	if err := removePrivateSessionState(s.store.Root(), planned.Plan.SessionID); err != nil {
		return session.Session{}, session.ErrOperationUncertain
	}
	sess, err = s.store.MarkSessionDiscarded(ctx, planned.Plan.SessionID)
	if err != nil {
		return session.Session{}, session.ErrOperationUncertain
	}
	if err := s.removeSessionReviewArtifacts(ctx, sess.ID); err != nil {
		return session.Session{}, session.ErrOperationUncertain
	}
	completed, err := s.completeDiscardOperation(ctx, op.ID, sess)
	if err != nil {
		return session.Session{}, session.ErrOperationUncertain
	}
	return completed, nil
}

func (s *SessionService) removeSessionReviewArtifacts(
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
		return session.Session{}, &session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail}
	}
	if op.State != session.OperationSucceeded {
		return session.Session{}, session.ErrOperationUncertain
	}
	var sess session.Session
	if err := json.Unmarshal(op.Result, &sess); err != nil {
		return session.Session{}, err
	}
	if sess.ID == "" {
		return session.Session{}, errors.New("decode session operation result: missing session id")
	}
	return sess, nil
}

func (s *SessionService) completeDiscardOperation(ctx context.Context, operationID string, sess session.Session) (session.Session, error) {
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

func (s *SessionService) CancelTurn(ctx context.Context, key string, req session.CancelTurnRequest) (session.Turn, error) {
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
	s.mu.Lock()
	active := s.active[req.TurnID]
	if active == nil {
		s.mu.Unlock()
		return session.Turn{}, &session.Error{Code: session.CodeOperationUncertain, Detail: "active turn worker is unavailable"}
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
			return session.Turn{}, err
		}
		if sessionTurnTerminal(observed.State) {
			return s.completeObservedCancel(context.Background(), op, observed)
		}
		return session.Turn{}, &session.Error{Code: session.CodeOperationUncertain, Detail: "active turn cleanup finished without a terminal result"}
	case <-timer.C:
		return session.Turn{}, &session.Error{Code: session.CodeOperationUncertain, Detail: "active turn cleanup is still pending"}
	case <-ctx.Done():
		return session.Turn{}, ctx.Err()
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

func (s *SessionService) failCancelOperation(ctx context.Context, op session.Operation, err error) error {
	if op.ID != "" {
		_ = s.store.FailOperation(ctx, op.ID, session.CodeOf(err), boundedSessionServiceError(err))
	}
	return err
}

func (s *SessionService) completeObservedCancel(ctx context.Context, op session.Operation, turn session.Turn) (session.Turn, error) {
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
		return session.Turn{}, &session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail}
	}
	if op.State != session.OperationSucceeded {
		return session.Turn{}, session.ErrOperationUncertain
	}
	var turn session.Turn
	if err := json.Unmarshal(op.Result, &turn); err != nil {
		return session.Turn{}, fmt.Errorf("decode cancel operation result: %w", err)
	}
	if turn.ID == "" {
		return session.Turn{}, errors.New("decode cancel operation result: missing turn id")
	}
	return turn, nil
}

func (s *SessionService) GetOperation(ctx context.Context, key string) (session.Operation, error) {
	return s.store.GetOperation(ctx, key)
}

func (s *SessionService) GetOperationByID(ctx context.Context, id string) (session.Operation, error) {
	return s.store.GetOperationByID(ctx, id)
}
