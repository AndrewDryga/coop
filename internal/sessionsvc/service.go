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
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	"github.com/AndrewDryga/coop/internal/workerproto"
)

const (
	// DefaultStopTimeout is how long the service waits for in-flight work to wind down, and the
	// budget a host's own HTTP shutdown should match.
	DefaultStopTimeout = 5 * time.Second
)

// errSessionForkUnproven marks a session whose workspace authority cannot be proved at start —
// its generation record or workspace is gone, or the fork was recreated under the same name. The
// daemon quarantines such a session (every operation on it keeps failing the live authority
// check) instead of refusing to start for everyone else; its durable history stays untouched.
var errSessionForkUnproven = errors.New("remote session workspace authority is unproven")

var errLegacySessionForkUnproven = fmt.Errorf("%w: legacy remote session has no store-bound fork ownership proof", errSessionForkUnproven)

const (
	sessionPolicyMaxWarmIdleTimeout = time.Hour
	sessionServiceCleanupInterval   = time.Minute
	sessionOperationStaleAfter      = 2 * time.Minute
	sessionCreateConcurrency        = 2
	runtimeCleanupBatchSize         = 2
	startupReapErrorLimit           = 8
)

// executionConfig is derived from one authenticated, immutable controller job.
// It is never loaded from a local registry or persisted as a second authority.
type executionConfig struct {
	Mode               agents.ExecutionMode
	Repository         string
	Companions         []executionCompanion
	Targets            []agents.Target
	OmitEnv            bool
	OmitMCP            bool
	RepositoryReadOnly bool
	Egress             executionNetwork
	MaxTurns           int
	MaxQueuedTurns     int
	MaxQueuedBytes     int
	TurnTimeout        time.Duration
	WarmIdleTimeout    time.Duration
	MaxPatchBytes      int
}

type executionNetwork struct {
	Mode               egress.Mode
	Rules              []egress.Rule
	ExportDestinations bool
}

type executionCompanion struct {
	Name       string
	Repository string
}

// sessionTargetList renders a ladder back to the target grammar.
func sessionTargetList(targets []agents.Target) string {
	parts := make([]string, len(targets))
	for i, target := range targets {
		parts[i] = target.String()
	}
	return strings.Join(parts, " ")
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

type CreateRemoteSessionRequest struct {
	Task              string                   `json:"task"`
	Job               json.RawMessage          `json:"job"`
	ExpectedJobDigest string                   `json:"expected_job_digest"`
	ControllerTools   *session.ControllerTools `json:"controller_tools,omitempty"`
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
	if err := s.validateSessionForkAuthority(ctx, sess); err != nil {
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

func cloneControllerTools(value *session.ControllerTools) *session.ControllerTools {
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

// SourceRefresher uses trusted host credentials to fetch only the saved job's default
// ref. It must leave the admitted source and its working tree unchanged.
type SourceRefresher func(context.Context, string, workerproto.JobSource, string) (string, error)

type Config struct {
	StateRoot           string
	SourceConfig        *config.Config
	Runtime             runtime.Runtime
	Executable          string
	Host                Host
	Runner              Runner
	RunnerFactory       RunnerFactory
	ReviewGate          ReviewGate
	SourceRefresher     SourceRefresher
	ReviewPublisher     ReviewPublisher
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
	store               *session.Store
	stateRoot           string
	sourceCfg           *config.Config
	rt                  runtime.Runtime
	executable          string
	host                Host
	runner              Runner
	reviewGate          ReviewGate
	sourceRefresher     SourceRefresher
	reviewPublisher     ReviewPublisher
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
	backgroundActive    map[string]bool
	backgroundSlots     chan struct{}
	testBeforeCreatePin func() error
	testAfterTurnLease  func(session.Turn)
	// testAdmitNetwork replaces create-time network admission. Real admission needs an owner
	// key, an approval and a Docker qualification; a test that only cares what the create path
	// does with the answer injects one. nil in production.
	testAdmitNetwork func(jobDigest, sessionID string, policy executionConfig, workspace, forkName string) (sessionNetworkBinding, error)
	// testSessionNetworkReads replaces the evidence read's registry reads: a retained run with
	// denials needs a qualified gateway execution nobody can create in a unit test. nil in production.
	testSessionNetworkReads func(bound session.Session, now time.Time) sessionNetworkReads
	runtimeMu               sync.Mutex
	runtimeLocks            map[string]*sessionOperationLock
	runtimeSlots            map[string]bool
	runtimeChanged          chan struct{}
	restoring               map[string]bool // sessions whose workspace a restore is rewriting right now
	testDuringRestoreFiles  func()          // test seam: runs while the restore holds the runtime and rewrites files
	runtimeCleanupMu        sync.Mutex
	runtimeCleanupCursor    int
	runtimeCleanupStampMu   sync.Mutex
	runtimeCleanupDone      map[string]runtimeCleanupStamp
	testBeforeCleanupStamp  func()
	testAfterStartupDrain   func()
	testStartupExecution    func(sessionID string)
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
		var err error
		sourceCfg, err = config.Load()
		if err != nil {
			return nil, err
		}
		box.ResolveBaseImage(sourceCfg)
	}
	store, err := session.Open(cfg.StateRoot)
	if err != nil {
		return nil, err
	}
	service := &Service{
		store: store, stateRoot: cfg.StateRoot,
		sourceCfg: sourceCfg,
		rt:        cfg.Runtime, executable: cfg.Executable, host: cfg.Host, runner: cfg.Runner,
		reviewGate:          cfg.ReviewGate,
		sourceRefresher:     cfg.SourceRefresher,
		reviewPublisher:     cfg.ReviewPublisher,
		stopTimeout:         cfg.StopTimeout,
		cleanupInterval:     cfg.CleanupInterval,
		operationStaleAfter: cfg.OperationStaleAfter,
		log:                 cfg.Logger,
		workers:             make(map[string]*sessionWorker), active: make(map[string]*activeSessionTurn),
		pendingCancels:   make(map[string]*pendingSessionCancel),
		quarantined:      make(map[string]struct{}),
		operationLocks:   make(map[string]*sessionOperationLock),
		backgroundActive: make(map[string]bool), backgroundSlots: make(chan struct{}, sessionCreateConcurrency),
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
	if runner, ok := service.runner.(*sessionTurnRunner); ok {
		runner.service = service
	}
	return service, nil
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
	retired, err := s.store.RetiredQuarantinedSessions(parent)
	if err != nil {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return err
	}
	s.historicalMu.Lock()
	for _, id := range retired {
		s.historicalPending[id] = struct{}{}
	}
	s.historicalMu.Unlock()
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
	_, hasRuntimeCustody := s.runner.(sessionRunnerRuntimeCleaner)
	for _, sess := range sessions {
		if hasRuntimeCustody && sess.State != session.SessionDiscarded && requireSessionForkAuthority(sess) == nil {
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
		if sess.State == session.SessionDiscarded {
			continue // never runs again; resolving its job cost git calls per row, past the start window
		}
		if _, isQuarantined := quarantined[sess.ID]; isQuarantined {
			continue // no worker for a session whose workspace authority is unproven
		}
		if s.testStartupExecution != nil {
			s.testStartupExecution(sess.ID)
		}
		if _, err := s.sessionExecution(ctx, sess); err != nil {
			continue // Historical rows remain readable; only saved jobs may execute.
		}
		if sess.QueuedTurnCount > 0 && requireSessionForkAuthority(sess) == nil {
			s.ensureWorkerLocked(sess.ID)
		}
	}
	for _, worker := range s.workers {
		s.triggerWorker(worker)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.drainHistoricalRuntimes(ctx)
		if s.testAfterStartupDrain != nil {
			s.testAfterStartupDrain()
		}
	}()
	s.mu.Unlock()
	return nil
}

// drainHistoricalRuntimes proves a restart's backlog without waiting for the ticker. Every session
// that outlived the previous daemon starts unproven, and capacity stays at zero until the last one
// is proven, so two proofs a minute kept a worker with a few dozen parked sessions unplaceable for
// tens of minutes. Batches stay bounded; a pass that proves nothing ends the drain and leaves what
// failed to the ticker, still unproven.
func (s *Service) drainHistoricalRuntimes(ctx context.Context) {
	for ctx.Err() == nil && s.historicalRuntimePending() {
		if s.cleanupIdleSessionRuntimes(ctx) == 0 {
			return
		}
	}
}

func (s *Service) historicalRuntimePending() bool {
	s.historicalMu.Lock()
	defer s.historicalMu.Unlock()
	return len(s.historicalPending) != 0
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
	if bound.ForkGeneration == "" {
		return session.Session{}, errLegacySessionForkUnproven
	}
	owned, err := s.store.OwnsSessionFork(ctx, bound.ID)
	if err != nil {
		return session.Session{}, err
	}
	if !owned {
		return session.Session{}, fmt.Errorf("%w: session has no owner-store binding", errSessionForkUnproven)
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
	current, reserved, err := forkspace.ReadWorkspaceReservation(bound.Repository, identity)
	if err != nil {
		return session.Session{}, err
	}
	if reserved && !current.MatchesSessionOwner(s.store.ID(), bound.ID) {
		return session.Session{}, fmt.Errorf("%w: workspace reservation has another or unproven owner", errSessionForkUnproven)
	}
	if !reserved {
		if err := forkspace.RequireForkNameAvailable(bound.Repository, bound.ForkName); err != nil {
			return session.Session{}, fmt.Errorf("%w: %v", errSessionForkUnproven, err)
		}
		if forkspace.NeedsStop(bound.Repository, bound.ForkName) {
			return session.Session{}, fmt.Errorf("%w: workspace runtime still needs cleanup", errSessionForkUnproven)
		}
		if err := forkspace.RequireNoForkExecutionsLocked(bound.Repository, identity); err != nil {
			return session.Session{}, fmt.Errorf("%w: %v", errSessionForkUnproven, err)
		}
	}
	reservation := forkspace.WorkspaceReservation{
		Version: forkspace.WorkspaceReservationVersion, Fork: identity,
		Kind: forkspace.WorkspaceReservationRemoteSession, OwnerStoreID: s.store.ID(),
		OwnerID: bound.ID, CreatedAt: time.Now().UTC(),
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
// host anchored generation record, workspace path, and durable remote-session reservation agree.
func (s *Service) validateSessionForkAuthority(ctx context.Context, bound session.Session) error {
	return s.validateSessionForkAuthorityState(ctx, bound, false)
}

func (s *Service) validateSessionForkAuthorityForDiscardPlan(ctx context.Context, bound session.Session) error {
	return s.validateSessionForkAuthorityState(ctx, bound, true)
}

func (s *Service) validateSessionForkAuthorityState(ctx context.Context, bound session.Session, allowMissingWorkspace bool) error {
	if bound.Repository == "" && bound.Workspace == "" && bound.ForkName == "" && bound.ForkGeneration == "" {
		return nil
	}
	if err := requireSessionForkAuthority(bound); err != nil {
		return err
	}
	owned, err := s.store.OwnsSessionFork(ctx, bound.ID)
	if err != nil {
		return err
	}
	if !owned {
		return errors.New("session has no owner-store binding")
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
	if !reserved || !reservation.MatchesSessionOwner(s.store.ID(), bound.ID) {
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
		detail += ": its execution mode is bare"
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

// cleanupIdleSessionRuntimes proves at most runtimeCleanupBatchSize idle runtimes and reports how
// many it proved clean.
func (s *Service) cleanupIdleSessionRuntimes(ctx context.Context) (proven int) {
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
		if err := s.validateSessionForkAuthority(ctx, candidate); err != nil {
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
			proven++
		}
		unlock()
		if getErr != nil && ctx.Err() == nil {
			s.host.warnf("could not clean session runtime state %s: %v", candidate.session.ID, getErr)
		}
	}
	s.runtimeCleanupCursor = (start + scanned) % len(candidates)
	return proven
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
	if err := s.validateSessionForkAuthority(ctx, bound); err != nil {
		return leased, err
	}
	turnCtx, err := s.sessionTurnContext(ctx, bound)
	if err != nil {
		return leased, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	return s.runner.Run(turnCtx, bound, leased)
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
	s.runtimeCleaned(sessionID)
}

// sessionTurnContext derives warm lifetime and target rotation from the saved job.
func (s *Service) sessionTurnContext(ctx context.Context, bound session.Session) (context.Context, error) {
	policy, err := s.sessionExecution(ctx, bound)
	if err != nil {
		return nil, err
	}
	if policy.WarmIdleTimeout > 0 {
		ctx = context.WithValue(ctx, sessionWarmIdleTimeoutContextKey{}, policy.WarmIdleTimeout)
	}
	if len(policy.Targets) > 1 {
		for _, rung := range policy.Targets {
			if rung.String() == bound.Target {
				return context.WithValue(ctx, sessionTargetLadderContextKey{}, policy.Targets), nil
			}
		}
	}
	return ctx, nil
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
	runner.service = s
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
		unlock, err := s.lockRuntimeCapacity(ctx, sessionID, func(bound session.Session) error {
			if bound.State == session.SessionClosed || bound.State == session.SessionDiscarded ||
				bound.Activity != session.ActivityParked || bound.ActiveTurnID != "" || bound.QueuedTurnCount == 0 {
				return &session.Error{Code: session.CodeTurnNotRunnable, Detail: "session has no runnable queued turn"}
			}
			return s.validateSessionForkAuthority(ctx, bound)
		})
		if err != nil {
			return
		}
		bound, err := s.store.GetSession(ctx, sessionID)
		if err != nil {
			s.finishRuntimeSlot(session.Session{ID: sessionID}, nil)
			unlock()
			return
		}
		if bound.State == session.SessionClosed || bound.State == session.SessionDiscarded {
			s.finishRuntimeSlot(bound, nil)
			unlock()
			return
		}
		if err := s.validateSessionForkAuthority(ctx, bound); err != nil {
			s.finishRuntimeSlot(bound, nil)
			unlock()
			return
		}
		leased, ok, err := s.store.LeaseNextTurn(ctx, sessionID)
		if err != nil || !ok {
			s.finishRuntimeSlot(bound, nil)
			unlock()
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
		s.finishRuntimeSlot(bound, runErr)
		unlock()
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

func (s *Service) CreateRemoteSession(ctx context.Context, key string, req CreateRemoteSessionRequest) (session.Session, error) {
	op, err := s.beginCreateOperation(ctx, key, req)
	if err != nil {
		return session.Session{}, err
	}
	if s.serviceRunning() {
		if op.State == session.OperationRunning {
			s.scheduleBackgroundOperation(op.ID)
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
		s.scheduleBackgroundOperation(op.ID)
	}
	return op, nil
}

func (s *Service) beginCreateOperation(
	ctx context.Context,
	key string,
	req CreateRemoteSessionRequest,
) (session.Operation, error) {
	if req.Task == "" || len(req.Task) > session.MaxExternalRefBytes || !utf8SessionText(req.Task) {
		return session.Operation{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "bounded task is required"}
	}
	job, err := workerproto.DecodeJobSpec(req.Job)
	if err != nil {
		return session.Operation{}, &session.Error{Code: session.CodeInvalidRequest, Detail: err.Error()}
	}
	if _, err := jobEgressRules(job); err != nil {
		return session.Operation{}, &session.Error{Code: session.CodeInvalidRequest, Detail: err.Error()}
	}
	if _, err := jobTargets(job); err != nil {
		return session.Operation{}, &session.Error{Code: session.CodeInvalidRequest, Detail: err.Error()}
	}
	digest, err := job.Digest()
	if err != nil || digest != req.ExpectedJobDigest {
		return session.Operation{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "job digest does not match the controller document"}
	}
	req.Job, err = job.CanonicalDocument()
	if err != nil {
		return session.Operation{}, &session.Error{Code: session.CodeInvalidRequest, Detail: err.Error()}
	}
	if err := session.ValidateControllerTools(req.ControllerTools); err != nil {
		return session.Operation{}, err
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
	intent, err := s.captureCreateIntent(ctx, op, req)
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
		sess, err := session.DecodeSessionOperationResult(op.Result)
		if err != nil {
			return session.Session{}, wrapServiceOperationError(op.ID,
				fmt.Errorf("decode create operation result: %w", err))
		}
		return sess, nil
	case session.OperationFailed:
		return session.Session{}, &serviceOperationError{
			operationID: op.ID,
			err:         &session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail},
		}
	case session.OperationRunning:
		var intent sessionCreateIntent
		decoder := json.NewDecoder(bytes.NewReader(op.Result))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&intent); err != nil {
			return s.rejectCreateIntent(ctx, op.ID, "create operation intent is unreadable")
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return s.rejectCreateIntent(ctx, op.ID, "create operation intent has trailing data")
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
	OperationID     string                   `json:"operation_id"`
	OwnerStoreID    string                   `json:"owner_store_id"`
	JobDocument     json.RawMessage          `json:"job_document"`
	JobDigest       string                   `json:"job_digest"`
	Task            string                   `json:"task"`
	SessionID       string                   `json:"session_id"`
	ForkName        string                   `json:"fork_name"`
	ControllerTools *session.ControllerTools `json:"controller_tools,omitempty"`
}

func (s *Service) captureCreateIntent(ctx context.Context, op session.Operation, req CreateRemoteSessionRequest) (sessionCreateIntent, error) {
	job, err := workerproto.DecodeJobSpec(req.Job)
	if err != nil {
		return sessionCreateIntent{}, err
	}
	execution, err := s.resolveJobExecution(ctx, job)
	if err != nil {
		return sessionCreateIntent{}, err
	}
	if req.ControllerTools != nil {
		if execution.Mode == agents.ModeBare {
			return sessionCreateIntent{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "a bare session cannot bind an MCP endpoint"}
		}
		if execution.Egress.Mode == egress.None {
			return sessionCreateIntent{}, &session.Error{Code: session.CodeInvalidRequest, Detail: controllerToolsNeedsNetwork}
		}
	}
	return sessionCreateIntent{
		OperationID: op.ID, OwnerStoreID: s.store.ID(), Task: req.Task,
		JobDocument: append(json.RawMessage(nil), req.Job...), JobDigest: req.ExpectedJobDigest,
		SessionID: deterministicSessionID(op.ID), ForkName: deterministicForkName(op.ID),
		ControllerTools: cloneControllerTools(req.ControllerTools),
	}, nil
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
	if intent.OwnerStoreID != s.store.ID() {
		return session.Session{}, s.makeOperationUncertain(ctx, op, "create operation store owner is unproven")
	}
	if intent.OperationID != op.ID || intent.SessionID != deterministicSessionID(op.ID) ||
		intent.ForkName != deterministicForkName(op.ID) {
		return s.rejectCreateIntent(ctx, op.ID, "create operation intent is invalid")
	}
	job, err := workerproto.DecodeJobSpec(intent.JobDocument)
	if err != nil {
		return s.rejectCreateIntent(ctx, op.ID, "create operation has no valid controller job authority; request a new session")
	}
	digest, err := job.Digest()
	if err != nil || digest != intent.JobDigest {
		return s.rejectCreateIntent(ctx, op.ID, "create operation job digest or reference is invalid")
	}
	execution, err := s.resolveJobExecution(ctx, job)
	if err != nil {
		return s.rejectCreateIntent(ctx, op.ID, "create operation job authority is unproven")
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
	expectedIntent := op.Result
	createReq := session.CreateSessionRequest{
		ID: intent.SessionID, ExternalRef: intent.Task, Target: execution.Targets[0].String(),
		JobDocument: append(json.RawMessage(nil), intent.JobDocument...), JobDigest: intent.JobDigest,
		Mode: string(execution.Mode), OmitEnv: execution.OmitEnv, OmitMCP: execution.OmitMCP,
		ControllerTools: cloneControllerTools(intent.ControllerTools), RepositoryReadOnly: execution.RepositoryReadOnly,
		MaxTurns: execution.MaxTurns, MaxQueuedTurns: execution.MaxQueuedTurns, MaxQueuedBytes: execution.MaxQueuedBytes,
		TurnTimeout: execution.TurnTimeout, MaxPatchBytes: execution.MaxPatchBytes,
	}
	var workspace sessionWorkspace
	var companions []session.CompanionRepository
	failCreate := func(cause error) (session.Session, error) {
		if ctx.Err() != nil && errors.Is(cause, ctx.Err()) {
			return session.Session{}, cause
		}
		if workspace.Path != "" {
			if cleanupErr := rollbackSessionCreate(workspace, companions); cleanupErr != nil {
				cause = errors.Join(cause, fmt.Errorf("rollback partial session creation: %w", cleanupErr))
			}
		}
		return session.Session{}, s.failServiceOperation(ctx, op.ID, cause)
	}
	if execution.Mode == agents.ModeBare {
		createReq.NetworkMode = string(execution.Egress.Mode)
	} else {
		if s.testBeforeCreatePin != nil {
			if err := s.testBeforeCreatePin(); err != nil {
				return failCreate(err)
			}
		}
		selected, base := emptyJobCommit, emptyJobCommit
		if job.Source != nil {
			selected, base = job.Source.Binding.SelectedCommit, job.Source.Binding.BaseCommit
			createReq.Source = session.CloneSourceBinding(&job.Source.Binding)
		}
		workspace, err = ensureSessionWorkspaceContext(ctx, s, execution.Repository, intent.ForkName, selected, intent.OwnerStoreID, intent.SessionID)
		if err != nil {
			if errors.Is(err, errSessionWorkspacePublicationPending) {
				// The generation or reservation may already be visible. Keep the
				// intent runnable to repeat the exact owner's durability barrier.
				return session.Session{}, wrapServiceOperationError(op.ID, err)
			}
			return failCreate(fmt.Errorf("ensure session workspace: %w", err))
		}
		for index, companion := range execution.Companions {
			path, err := sessionCompanionWorkspace(s.store.Root(), intent.SessionID, companion.Name)
			if err != nil {
				return failCreate(err)
			}
			resolved, err := ensureSessionCompanionContext(ctx, s.store.Root(), intent.SessionID, session.CompanionRepository{
				Name: companion.Name, Repository: companion.Repository, Workspace: path,
				BaseCommit: job.Companions[index].Source.Binding.SelectedCommit,
			})
			if err != nil {
				return failCreate(fmt.Errorf("ensure companion %q: %w", companion.Name, err))
			}
			companions = append(companions, resolved)
		}
		network, err := s.admitControllerJobNetwork(intent.JobDigest, intent.SessionID, execution, workspace.Path, intent.ForkName, companions)
		if err != nil {
			return failCreate(&session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()})
		}
		if execution.Mode.Restricted() && network.Mode == egress.Filtered {
			return failCreate(&session.Error{Code: session.CodeNetworkUnavailable, Detail: "restricted sessions do not support filtered networking"})
		}
		createReq.Repository, createReq.Workspace, createReq.ForkName = execution.Repository, workspace.Path, intent.ForkName
		createReq.ForkGeneration = string(workspace.Fork.Generation)
		createReq.BaseCommit, createReq.Companions = base, companions
		createReq.RepositoryFreshness = jobRepositoryFreshness(job, op.CreatedAt)
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
	var bound session.Session
	var policy executionConfig
	unlock, err := s.lockRuntimeCapacity(ctx, id, func(current session.Session) error {
		var err error
		bound = current
		policy, err = s.prepareSessionExecution(ctx, bound, expectedRevision)
		return err
	})
	if err != nil {
		return session.Session{}, err
	}
	defer unlock()
	defer func() { s.finishRuntimeSlot(bound, nil) }()
	if inspector, ok := s.runner.(sessionRunnerWarmInspector); ok && inspector.WarmSessionReady(bound) {
		return bound, nil
	}
	preparer := s.runner.(sessionRunnerPreparer)
	prepareCtx, cancel := context.WithTimeout(ctx, policy.TurnTimeout)
	defer cancel()
	// Even a failed Prepare may have launched a child whose cleanup needs retry.
	s.invalidateRuntimeCleanup(id)
	if err := preparer.PrepareSession(prepareCtx, bound, policy.WarmIdleTimeout); err != nil {
		if sessionRunnerCleanupFailed(err) {
			s.runtimeCleanupFailed(id)
		}
		return session.Session{}, err
	}
	return s.store.GetSession(ctx, id)
}

func (s *Service) prepareSessionExecution(ctx context.Context, bound session.Session, expectedRevision int64) (executionConfig, error) {
	if err := s.validateSessionForkAuthority(ctx, bound); err != nil {
		return executionConfig{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	policy, err := s.sessionExecution(ctx, bound)
	if err != nil {
		return executionConfig{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	if bound.Revision != expectedRevision {
		if inspector, ok := s.runner.(sessionRunnerWarmInspector); ok && inspector.WarmSessionReady(bound) {
			return policy, nil
		}
		return executionConfig{}, &session.Error{
			Code:   session.CodeRevisionConflict,
			Detail: fmt.Sprintf("expected revision %d, current revision %d", expectedRevision, bound.Revision),
		}
	}
	if bound.State != session.SessionOpen || bound.Activity != session.ActivityParked ||
		bound.ActiveTurnID != "" || bound.QueuedTurnCount != 0 {
		return executionConfig{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: "session must be open and idle before it can be prepared"}
	}
	if policy.WarmIdleTimeout <= 0 {
		return executionConfig{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: "session job does not enable warm execution"}
	}
	if _, ok := s.runner.(sessionRunnerPreparer); !ok {
		return executionConfig{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: "session runner does not support warm execution"}
	}
	return policy, nil
}

func (s *Service) ListSessions(ctx context.Context, limit int) ([]session.Session, error) {
	return s.store.ListSessions(ctx, limit)
}

// SubmitTurn admits a turn without taking the session's runtime lock: admission is one store
// transaction fenced by expected_revision, and the fork-authority check takes its own lock. The
// runtime lock belongs to whoever runs, reviews, or cleans up the workspace — a submit made while a
// turn is executing must queue behind it, never wait on the line for it to finish.
func (s *Service) SubmitTurn(ctx context.Context, key string, req session.SubmitTurnRequest) (session.Turn, error) {
	if pending, err := s.store.HasPendingWorkspaceRestore(ctx, req.SessionID, ""); err != nil || pending {
		return session.Turn{}, &session.Error{Code: session.CodeInvalidSessionState,
			Detail: "a workspace restore is in progress on this session; retry once it completes"}
	}
	if err := s.validateTurnEscalation(ctx, req); err != nil {
		return session.Turn{}, err
	}
	// Serialize the final check and durable admission with beginWorkspaceRestore.
	// Taking the runtime lock instead would block queued turns behind a running model.
	s.runtimeMu.Lock()
	if s.restoring[req.SessionID] {
		s.runtimeMu.Unlock()
		// Refused before any receipt is journaled, so the same key succeeds once the restore is
		// done and the turn then runs on the restored workspace it was meant for.
		return session.Turn{}, &session.Error{Code: session.CodeInvalidSessionState,
			Detail: "a workspace restore is in progress on this session; retry once it completes"}
	}
	turn, err := s.store.SubmitTurn(ctx, key, req)
	s.runtimeMu.Unlock()
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
	if err := s.validateSessionForkAuthority(ctx, bound); err != nil {
		return &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	if bound.NetworkMode == string(egress.None) && req.ControllerTools != nil {
		return &session.Error{Code: session.CodeInvalidRequest, Detail: controllerToolsNeedsNetwork}
	}
	if err := validateRestrictedTurn(bound, req); err != nil {
		return err
	}
	policy, err := s.sessionExecution(ctx, bound)
	if err != nil {
		return &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	if req.MinTargetIndex <= 0 {
		return nil
	}
	if req.MinTargetIndex >= len(policy.Targets) {
		return &session.Error{Code: session.CodeInvalidRequest, Detail: fmt.Sprintf(
			"min_target_index %d is not a rung of this session's %d-rung target ladder",
			req.MinTargetIndex, len(policy.Targets),
		)}
	}
	return nil
}

// controllerToolsNeedsNetwork is shared by both admissions: a controller endpoint is reached
// by URL, and an offline box reaches none.
const controllerToolsNeedsNetwork = "an offline session binds no controller MCP endpoint — its box reaches no server by URL"

// validateRestrictedTurn refuses, at admission, the two turn options a restricted session cannot
// honor. Each of its turns runs in a fresh box whose provider history dies with it, so there is
// no native session for a caller's semantic rejection to re-prompt; and a bare session runs no
// tool, so a controller endpoint bound to its turn would be authority nothing can use.
func validateRestrictedTurn(bound session.Session, req session.SubmitTurnRequest) error {
	if !agents.ExecutionMode(bound.Mode).Restricted() {
		return nil
	}
	if req.OutputContract != nil && req.OutputContract.RequireSemanticValidation {
		return &session.Error{Code: session.CodeInvalidRequest, Detail: fmt.Sprintf(
			"a %s session keeps no provider history to re-prompt, so require_semantic_validation is not available; validate the completed turn's assistant message instead", bound.Mode)}
	}
	if bound.Mode == string(agents.ModeBare) && req.ControllerTools != nil {
		return &session.Error{Code: session.CodeInvalidRequest, Detail: "a bare session binds no controller MCP endpoint — it runs no tool"}
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
	if err := s.validateSessionForkAuthority(ctx, bound); err != nil {
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
	s.markHistoricalRuntimeClean(sessionID)
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
	if err := s.validateSessionForkAuthority(ctx, current); err != nil {
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
			s.runtimeCleanupFailed(current.ID)
			return session.Session{}, err
		}
		s.markHistoricalRuntimeClean(current.ID)
	} else if cleaner, ok := s.runner.(sessionRunnerRuntimeCleaner); ok {
		if err := cleaner.CleanupSession(ctx, current); err != nil {
			s.runtimeCleanupFailed(current.ID)
			return session.Session{}, err
		}
		s.markHistoricalRuntimeClean(current.ID)
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
	if err := s.validateSessionForkAuthority(ctx, sess); err != nil {
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
	if err := s.validateSessionForkAuthority(ctx, sess); err != nil {
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
	if err := s.validateSessionForkAuthorityForDiscardPlan(ctx, sess); err != nil {
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
	release := s.lockSessionRuntime(req.SessionID)
	defer release()
	if err := s.requireNoPendingPublication(ctx, req.SessionID); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
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
	s.historicalMu.Lock()
	s.historicalPending[sess.ID] = struct{}{}
	s.historicalMu.Unlock()
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
	release := s.lockSessionRuntime(planned.Plan.SessionID)
	defer release()
	if err := s.requireNoPendingPublication(ctx, planned.Plan.SessionID); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
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
	if sess.Workspace != "" {
		owned, ownerErr := s.store.OwnsSessionFork(ctx, sess.ID)
		if ownerErr != nil {
			return session.Session{}, s.failServiceOperation(ctx, op.ID, ownerErr)
		}
		if !owned {
			return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeDiscardPlanStale, Detail: "session has no owner-store binding"})
		}
	}
	if err := validateDiscardSessionBinding(sess, planned.Plan, s.store.ID()); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{Code: session.CodeDiscardPlanStale, Detail: boundedSessionServiceError(err)})
	}
	if op.State == session.OperationReserved {
		if err := s.store.MarkOperationRunning(ctx, op.ID, intentData); err != nil {
			return session.Session{}, err
		}
	}
	if sess.State == session.SessionDiscarded {
		if err := s.removeSessionArtifacts(ctx, sess.ID); err != nil {
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
	// A controller job's boxes never start its repository's services (box.RunSpec.ControllerJob), so
	// only a historical session can own a Compose project to remove. The job's project file is
	// repository code: it must not decide, or fail, a worker's cleanup.
	if s.rt.Name != "" && sess.JobDigest == "" {
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
	if err := s.removeSessionArtifacts(ctx, sess.ID); err != nil {
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

func validateDiscardSessionBinding(sess session.Session, plan DiscardPlan, storeID string) error {
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
	if workspace.Reservation == nil || workspace.Reservation.Fork != expected ||
		!workspace.Reservation.MatchesSessionOwner(storeID, sess.ID) {
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

func (s *Service) removeSessionArtifacts(
	ctx context.Context,
	sessionID string,
) error {
	if err := s.removeSessionCheckpointArtifacts(ctx, sessionID); err != nil {
		return err
	}
	operationIDs, err := s.store.ListSessionArtifactOperationIDs(ctx, "RunReview", sessionID)
	if err != nil {
		return err
	}
	root := filepath.Join(s.stateRoot, "review-artifacts")
	for _, operationID := range operationIDs {
		if !validSessionPathComponent(operationID) {
			return errors.New("stored review operation has an invalid artifact identity")
		}
		op, err := s.store.GetOperationByID(ctx, operationID)
		if err != nil {
			return err
		}
		if op.State == session.OperationRunning || op.State == session.OperationUncertain {
			if err := s.store.FailOperation(ctx, operationID, session.CodeInvalidSessionState, "review session was discarded"); err != nil {
				return err
			}
		}
		path := filepath.Join(root, operationID+".diff")
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove review patch artifact: %w", err)
		}
		gateOutput, err := s.reviewGateOutputPath(operationID)
		if err != nil {
			return err
		}
		if err := os.Remove(gateOutput); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove review gate output: %w", err)
		}
		candidate, err := s.reviewCandidatePath(operationID)
		if err != nil {
			return err
		}
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe retained review candidate path")
		}
		if err := os.RemoveAll(candidate); err != nil {
			return fmt.Errorf("remove reviewed candidate: %w", err)
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
	sess, err := session.DecodeSessionOperationResult(op.Result)
	if err != nil {
		return session.Session{}, wrapServiceOperationError(op.ID, err)
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
	return errors.Join(
		removePrivateSessionDir(filepath.Join(stateRoot, "acp"), sessionID),
		removePrivateSessionDir(filepath.Join(stateRoot, "output"), sessionID),
	)
}

func removePrivateSessionDir(root, sessionID string) error {
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
		return fmt.Errorf("inspect private session directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("private session directory is ambiguous")
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove private session directory: %w", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("private session directory remains after removal")
		}
		return fmt.Errorf("verify private session directory removal: %w", err)
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
	if err := s.validateSessionForkAuthority(ctx, bound); err != nil {
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
