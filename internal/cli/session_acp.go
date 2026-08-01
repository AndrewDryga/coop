package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/session"
)

const (
	sessionACPFrameLimit      = 12 << 20
	sessionACPTranscriptLimit = 4 << 20
	sessionACPMessageLimit    = session.MaxEventPayloadBytes
	sessionACPArtifactLimit   = 1 << 20
	sessionACPTermGrace       = 250 * time.Millisecond
	sessionACPKillGrace       = 750 * time.Millisecond
	sessionACPCleanupTimeout  = 2 * time.Second
	sessionACPWarmLimit       = 20
)

const (
	sessionACPProtocolError   session.ErrorCode = "acp_protocol_error"
	sessionACPProcessError    session.ErrorCode = "acp_process_error"
	sessionACPTimeoutError    session.ErrorCode = "acp_timeout"
	sessionACPCancelledError  session.ErrorCode = "acp_cancelled"
	sessionACPCredentialError session.ErrorCode = "credential_projection_error"
	sessionACPCleanupError    session.ErrorCode = "session_cleanup_error"
	sessionACPInvalidTarget   session.ErrorCode = "invalid_session_target"
	sessionACPInvalidTurn     session.ErrorCode = "invalid_leased_turn"
)

type sessionACPCommand func(string, ...string) *exec.Cmd

type sessionCancelRequestContextKey struct{}
type sessionWarmIdleTimeoutContextKey struct{}

type sessionWarmExecution struct {
	bound              session.Session
	child              *sessionACPProcess
	projection         *sessionACPProjection
	credentialDeadline time.Time
	expiresAt          time.Time
	timer              *time.Timer
}

// sessionTurnRunner owns boxed ACP children. Policy-opted sessions can retain one authenticated
// child across serialized turns; the same Coop fork path still owns box assembly and labels.
type sessionTurnRunner struct {
	sourceCfg  *config.Config
	stateRoot  string
	store      *session.Store
	rt         runtime.Runtime
	executable string
	command    sessionACPCommand
	warmMu     sync.Mutex
	warm       map[string]*sessionWarmExecution
	warming    int
}

func newSessionTurnRunner(sourceCfg *config.Config, stateRoot string, store *session.Store, rt runtime.Runtime, executable string, command ...sessionACPCommand) *sessionTurnRunner {
	start := sessionACPCommand(exec.Command)
	if len(command) > 0 && command[0] != nil {
		start = command[0]
	}
	return &sessionTurnRunner{
		sourceCfg: sourceCfg, stateRoot: stateRoot,
		store: store, rt: rt, executable: executable, command: start,
		warm: make(map[string]*sessionWarmExecution),
	}
}

// Run executes exactly one turn returned by Store.LeaseNextTurn. It never leases another turn.
func (r *sessionTurnRunner) Run(ctx context.Context, bound session.Session, leased session.Turn) (result session.Turn, runErr error) {
	var execution *sessionWarmExecution
	var assistant string
	var outputArtifacts []session.OutputArtifact
	protocolComplete := false
	parked := false
	warmIdleTimeout, _ := ctx.Value(sessionWarmIdleTimeoutContextKey{}).(time.Duration)

	defer func() {
		var cleanup []error
		cleanupFailed := false
		baseErr := runErr
		if protocolComplete && baseErr == nil && warmIdleTimeout > 0 && execution != nil {
			parked = r.parkWarmExecution(execution, warmIdleTimeout)
			if parked {
				execution = nil
			}
		}
		if execution != nil && execution.child != nil {
			cleanup = append(cleanup, execution.child.stop())
			cleanup = append(cleanup, r.removeTurnBox(execution.child.runID))
			cleanup = append(cleanup, r.stopSessionServices(context.Background(), bound))
		}
		if execution != nil && execution.projection != nil {
			cleanup = append(cleanup, execution.projection.remove())
		}

		for _, err := range cleanup {
			if err != nil {
				cleanupFailed = true
				if baseErr == nil {
					baseErr = acpFailure(sessionACPCleanupError, "turn cleanup failed")
				}
				baseErr = errors.Join(baseErr, err)
			}
		}
		if protocolComplete && baseErr == nil {
			completed, err := r.completeTurn(bound, leased, assistant, outputArtifacts)
			if err != nil {
				baseErr = err
				if parked {
					baseErr = errors.Join(baseErr, r.evictWarmExecution(bound.ID))
				}
			} else {
				result = completed
			}
		}
		if !protocolComplete && !cleanupFailed && errors.Is(ctx.Err(), context.Canceled) && baseErr != nil {
			if lookup, ok := ctx.Value(sessionCancelRequestContextKey{}).(func() (string, session.CancelTurnRequest, bool)); ok {
				if key, request, requested := lookup(); requested && key != "" {
					cancelCtx, cancel := context.WithTimeout(context.Background(), sessionACPCleanupTimeout)
					cancelled, err := r.store.CancelTurn(cancelCtx, key, request)
					cancel()
					if err == nil {
						result, baseErr = cancelled, nil
					} else {
						baseErr = err
					}
				}
			}
		}
		if baseErr != nil {
			result = r.failTurn(bound, leased, baseErr, result)
		}
		runErr = baseErr
	}()

	if r.store == nil || r.sourceCfg == nil {
		runErr = acpFailure(sessionACPInvalidTurn, "runner is not configured")
		return result, runErr
	}
	if bound.ID == "" || leased.ID == "" || bound.ID != leased.SessionID ||
		(leased.State != session.TurnStarting && leased.State != session.TurnRunning) ||
		leased.SendState != session.SendStateNone {
		runErr = acpFailure(sessionACPInvalidTurn, "turn is not an un-sent leased turn")
		return result, runErr
	}
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		runErr = acpFailure(sessionACPTimeoutError, "turn deadline is required")
		return result, runErr
	}
	if err := ctx.Err(); err != nil {
		runErr = classifyContextFailure(err)
		return result, runErr
	}

	target, err := agents.ParseTarget(bound.Target)
	if err != nil || len(target.Accounts) > 1 {
		runErr = acpFailure(sessionACPInvalidTarget, "session target must be one explicit provider and account")
		return result, runErr
	}
	agent, ok := agents.Get(target.Provider)
	if !ok {
		runErr = acpFailure(sessionACPInvalidTarget, "session target provider is unavailable")
		return result, runErr
	}

	if warmIdleTimeout > 0 {
		execution = r.takeWarmExecution(bound)
	} else {
		_ = r.evictWarmExecution(bound.ID)
	}
	if execution == nil {
		credentialDeadline := deadline
		if warmIdleTimeout > 0 {
			credentialDeadline = time.Now().Add(warmIdleTimeout + bound.TurnTimeout)
		}
		projection, err := r.projectCredentials(bound, target, agent, credentialDeadline)
		if err != nil && projection != nil {
			_ = projection.remove()
		}
		if err != nil && warmIdleTimeout > 0 {
			warmIdleTimeout = 0
			credentialDeadline = deadline
			projection, err = r.projectCredentials(bound, target, agent, credentialDeadline)
		}
		if err != nil {
			if projection != nil {
				_ = projection.remove()
			}
			runErr = err
			return result, runErr
		}
		child, err := r.startChild(ctx, bound, leased, projection.privateRoot)
		if err != nil {
			_ = projection.remove()
			runErr = err
			return result, runErr
		}
		execution = &sessionWarmExecution{bound: bound, child: child, projection: projection, credentialDeadline: credentialDeadline}
	}

	assistant, outputArtifacts, err = r.runACP(ctx, execution.child, bound, leased)
	if err != nil {
		runErr = err
		return result, runErr
	}
	protocolComplete = true
	return result, nil
}

type sessionACPFailure struct {
	code   session.ErrorCode
	detail string
}

func (e *sessionACPFailure) Error() string { return string(e.code) + ": " + e.detail }

func acpFailure(code session.ErrorCode, detail string) error {
	if code == "" {
		code = session.CodeInternal
	}
	if detail == "" {
		detail = "session turn failed"
	}
	return &sessionACPFailure{code: code, detail: detail}
}

func classifyContextFailure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return acpFailure(sessionACPTimeoutError, "turn deadline exceeded")
	}
	return acpFailure(sessionACPCancelledError, "turn cancelled")
}

func (r *sessionTurnRunner) completeTurn(bound session.Session, leased session.Turn, assistant string, artifacts []session.OutputArtifact) (session.Turn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sessionACPCleanupTimeout)
	defer cancel()
	return r.store.CompleteTurn(ctx, session.CompleteTurnRequest{SessionID: bound.ID, TurnID: leased.ID, Message: assistant, Artifacts: artifacts})
}

func (r *sessionTurnRunner) failTurn(bound session.Session, leased session.Turn, cause error, current session.Turn) session.Turn {
	if r.store == nil || bound.ID == "" || leased.ID == "" || leased.SessionID != bound.ID {
		return current
	}
	code := sessionACPProcessError
	detail := "ACP child failed"
	var failure *sessionACPFailure
	if errors.As(cause, &failure) {
		code, detail = failure.code, failure.detail
	} else if errors.Is(cause, context.DeadlineExceeded) {
		code, detail = sessionACPTimeoutError, "turn deadline exceeded"
	} else if errors.Is(cause, context.Canceled) {
		code, detail = sessionACPCancelledError, "turn cancelled"
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionACPCleanupTimeout)
	defer cancel()
	failed, err := r.store.FailTurn(ctx, session.FailTurnRequest{SessionID: bound.ID, TurnID: leased.ID, ErrorCode: code, ErrorDetail: detail})
	if err != nil {
		return current
	}
	return failed
}

func (r *sessionTurnRunner) removeTurnBox(runID string) error {
	if runID == "" || r.rt.Name == "" {
		if runID != "" {
			return acpFailure(sessionACPCleanupError, "runtime cleanup is unavailable")
		}
		return nil
	}
	if !validSessionRunID(runID) {
		return acpFailure(sessionACPCleanupError, "runtime cleanup is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionACPCleanupTimeout)
	defer cancel()
	_, err := r.rt.RemoveByLabel(ctx, box.LabelRun, runID)
	if err != nil {
		return acpFailure(sessionACPCleanupError, "runtime cleanup failed")
	}
	return nil
}

func (r *sessionTurnRunner) ReapInterruptedTurn(ctx context.Context, bound session.Session, turn session.Turn) error {
	if turn.SessionID == "" || turn.ID == "" {
		return acpFailure(sessionACPCleanupError, "interrupted turn identity is unavailable")
	}
	return errors.Join(
		r.removeTurnBox(sessionTurnRunID(turn.SessionID, turn.ID)),
		r.stopSessionServices(ctx, bound),
	)
}

func sessionWarmExecutionMatches(execution *sessionWarmExecution, bound session.Session) bool {
	if execution == nil || execution.child == nil || execution.projection == nil ||
		execution.child.exited.Load() {
		return false
	}
	return execution.bound.ID == bound.ID &&
		execution.bound.Target == bound.Target &&
		execution.bound.PolicyDigest == bound.PolicyDigest &&
		execution.bound.Repository == bound.Repository &&
		execution.bound.Workspace == bound.Workspace &&
		execution.bound.ForkName == bound.ForkName
}

func (r *sessionTurnRunner) takeWarmExecution(bound session.Session) *sessionWarmExecution {
	r.warmMu.Lock()
	execution := r.warm[bound.ID]
	if execution != nil {
		delete(r.warm, bound.ID)
		if execution.timer != nil {
			execution.timer.Stop()
			execution.timer = nil
		}
	}
	r.warmMu.Unlock()
	if execution == nil {
		return nil
	}
	if time.Now().Before(execution.expiresAt) && sessionWarmExecutionMatches(execution, bound) {
		return execution
	}
	_ = r.cleanupWarmExecution(execution)
	return nil
}

func (r *sessionTurnRunner) parkWarmExecution(execution *sessionWarmExecution, idleTimeout time.Duration) bool {
	if execution == nil || idleTimeout <= 0 || !sessionWarmExecutionMatches(execution, execution.bound) {
		return false
	}
	expiresAt := time.Now().Add(idleTimeout)
	credentialLimit := execution.credentialDeadline.Add(-execution.bound.TurnTimeout)
	if credentialLimit.Before(expiresAt) {
		expiresAt = credentialLimit
	}
	if !expiresAt.After(time.Now()) {
		return false
	}
	r.warmMu.Lock()
	if _, exists := r.warm[execution.bound.ID]; !exists && len(r.warm) >= sessionACPWarmLimit {
		r.warmMu.Unlock()
		return false
	}
	previous := r.warm[execution.bound.ID]
	execution.expiresAt = expiresAt
	execution.timer = time.AfterFunc(time.Until(expiresAt), func() {
		r.expireWarmExecution(execution.bound.ID, execution)
	})
	r.warm[execution.bound.ID] = execution
	r.warmMu.Unlock()
	if previous != nil && previous != execution {
		_ = r.cleanupWarmExecution(previous)
	}
	return true
}

func (r *sessionTurnRunner) expireWarmExecution(sessionID string, expected *sessionWarmExecution) {
	r.warmMu.Lock()
	if r.warm[sessionID] != expected {
		r.warmMu.Unlock()
		return
	}
	delete(r.warm, sessionID)
	expected.timer = nil
	r.warmMu.Unlock()
	_ = r.cleanupWarmExecution(expected)
}

func (r *sessionTurnRunner) evictWarmExecution(sessionID string) error {
	r.warmMu.Lock()
	execution := r.warm[sessionID]
	delete(r.warm, sessionID)
	if execution != nil && execution.timer != nil {
		execution.timer.Stop()
		execution.timer = nil
	}
	r.warmMu.Unlock()
	return r.cleanupWarmExecution(execution)
}

func (r *sessionTurnRunner) cleanupWarmExecution(execution *sessionWarmExecution) error {
	if execution == nil {
		return nil
	}
	var errs []error
	if execution.child != nil {
		errs = append(errs, execution.child.stop())
		errs = append(errs, r.removeTurnBox(execution.child.runID))
		errs = append(errs, r.stopSessionServices(context.Background(), execution.bound))
	}
	if execution.projection != nil {
		errs = append(errs, execution.projection.remove())
	}
	return errors.Join(errs...)
}

func (r *sessionTurnRunner) PrepareSession(ctx context.Context, bound session.Session, idleTimeout time.Duration) error {
	if idleTimeout <= 0 || idleTimeout > sessionPolicyMaxWarmIdleTimeout {
		return acpFailure(sessionACPInvalidTurn, "warm idle timeout is invalid")
	}
	r.warmMu.Lock()
	existing := r.warm[bound.ID]
	ready := existing != nil && time.Now().Before(existing.expiresAt) && sessionWarmExecutionMatches(existing, bound)
	if existing != nil && !ready {
		delete(r.warm, bound.ID)
		if existing.timer != nil {
			existing.timer.Stop()
			existing.timer = nil
		}
	}
	full := !ready && len(r.warm)+r.warming >= sessionACPWarmLimit
	if !ready && !full {
		r.warming++
	}
	r.warmMu.Unlock()
	if existing != nil && !ready {
		_ = r.cleanupWarmExecution(existing)
	}
	if ready {
		return nil
	}
	if full {
		return acpFailure(sessionACPProcessError, "warm session limit reached")
	}
	defer func() {
		r.warmMu.Lock()
		r.warming--
		r.warmMu.Unlock()
	}()
	target, err := agents.ParseTarget(bound.Target)
	if err != nil || len(target.Accounts) > 1 {
		return acpFailure(sessionACPInvalidTarget, "session target must be one explicit provider and account")
	}
	agent, ok := agents.Get(target.Provider)
	if !ok {
		return acpFailure(sessionACPInvalidTarget, "session target provider is unavailable")
	}
	credentialDeadline := time.Now().Add(idleTimeout + bound.TurnTimeout)
	projection, err := r.projectCredentials(bound, target, agent, credentialDeadline)
	if err != nil {
		if projection != nil {
			_ = projection.remove()
		}
		return err
	}
	child, err := r.startChildWithRunID(ctx, bound, sessionWarmRunID(bound.ID), projection.privateRoot)
	if err != nil {
		_ = projection.remove()
		return err
	}
	execution := &sessionWarmExecution{bound: bound, child: child, projection: projection, credentialDeadline: credentialDeadline}
	if _, _, err := r.runACP(ctx, child, bound, session.Turn{}); err != nil {
		return errors.Join(err, r.cleanupWarmExecution(execution))
	}
	if current, err := r.store.GetSession(ctx, bound.ID); err == nil {
		execution.bound = current
	} else {
		return errors.Join(err, r.cleanupWarmExecution(execution))
	}
	if !r.parkWarmExecution(execution, idleTimeout) {
		return errors.Join(acpFailure(sessionACPProcessError, "warm execution could not be retained"), r.cleanupWarmExecution(execution))
	}
	return nil
}

func (r *sessionTurnRunner) WarmSessionReady(bound session.Session) bool {
	r.warmMu.Lock()
	defer r.warmMu.Unlock()
	execution := r.warm[bound.ID]
	return execution != nil && time.Now().Before(execution.expiresAt) &&
		sessionWarmExecutionMatches(execution, bound)
}

func (r *sessionTurnRunner) CleanupParkedSession(ctx context.Context, bound session.Session) error {
	r.warmMu.Lock()
	execution := r.warm[bound.ID]
	ready := execution != nil && time.Now().Before(execution.expiresAt) && sessionWarmExecutionMatches(execution, bound)
	r.warmMu.Unlock()
	if ready {
		return nil
	}
	return r.cleanupKnownSessionRuntime(ctx, bound)
}

func (r *sessionTurnRunner) CleanupClosedSession(ctx context.Context, bound session.Session) error {
	return r.cleanupKnownSessionRuntime(ctx, bound)
}

func (r *sessionTurnRunner) CloseWarmSessions() error {
	r.warmMu.Lock()
	executions := make([]*sessionWarmExecution, 0, len(r.warm))
	for id, execution := range r.warm {
		delete(r.warm, id)
		if execution.timer != nil {
			execution.timer.Stop()
			execution.timer = nil
		}
		executions = append(executions, execution)
	}
	r.warmMu.Unlock()
	var errs []error
	for _, execution := range executions {
		errs = append(errs, r.cleanupWarmExecution(execution))
	}
	return errors.Join(errs...)
}

type sessionACPProjection struct {
	privateRoot string
	files       []string
}

func (p *sessionACPProjection) remove() error {
	if p == nil {
		return nil
	}
	var errs []error
	for _, path := range p.files {
		if err := removeProjectedSessionFile(path); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(acpFailure(sessionACPCleanupError, "projected credential cleanup failed"), errors.Join(errs...))
	}
	return nil
}

func removeProjectedSessionFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return errors.New("projected session file is not regular")
	}
	return os.Remove(path)
}

func (r *sessionTurnRunner) projectCredentials(bound session.Session, target agents.Target, agent agents.Agent, deadline time.Time) (*sessionACPProjection, error) {
	if r.stateRoot == "" || !validSessionPathComponent(bound.ID) {
		return nil, acpFailure(sessionACPCredentialError, "private credential root is unavailable")
	}
	resolvedStateRoot, err := filepath.EvalSymlinks(r.stateRoot)
	if err != nil || !filepath.IsAbs(resolvedStateRoot) {
		return nil, acpFailure(sessionACPCredentialError, "private credential root is invalid")
	}
	privateRoot := filepath.Join(resolvedStateRoot, "acp", bound.ID)
	privateRoot, err = filepath.Abs(privateRoot)
	if err != nil || !filepath.IsAbs(privateRoot) {
		return nil, acpFailure(sessionACPCredentialError, "private credential root is invalid")
	}
	if err := ensurePrivateDirectory(privateRoot); err != nil {
		return nil, acpFailure(sessionACPCredentialError, "private credential root is unsafe")
	}

	account := target.Account()
	if account == "" {
		account = r.sourceCfg.DefaultProfileOf(target.Provider)
	}
	if !validSessionPathComponent(account) {
		return nil, acpFailure(sessionACPCredentialError, "selected credential account is invalid")
	}
	if r.sourceCfg.ConfigDir == "" {
		return nil, acpFailure(sessionACPCredentialError, "source credential root is invalid")
	}
	sourceRoot, err := filepath.Abs(r.sourceCfg.ConfigDir)
	if err != nil || sourceRoot == "" {
		return nil, acpFailure(sessionACPCredentialError, "source credential root is invalid")
	}
	sourceRoot, err = filepath.EvalSymlinks(sourceRoot)
	if err != nil || !filepath.IsAbs(sourceRoot) {
		return nil, acpFailure(sessionACPCredentialError, "source credential root is invalid")
	}
	if sameOrBelow(privateRoot, sourceRoot) || sameOrBelow(sourceRoot, privateRoot) {
		return nil, acpFailure(sessionACPCredentialError, "private credential root overlaps source credentials")
	}
	projection := &sessionACPProjection{privateRoot: privateRoot}
	if err := r.projectSessionConfigFiles(sourceRoot, projection); err != nil {
		return projection, err
	}
	sourceProfile := filepath.Join(sourceRoot, target.Provider, "profiles", account)
	privateProfile := filepath.Join(privateRoot, target.Provider, "profiles", account)
	if err := ensurePrivateDirectory(privateProfile); err != nil {
		return projection, acpFailure(sessionACPCredentialError, "private credential account is unsafe")
	}
	instructionFile := agent.InstructionFile()
	if !validArtifactName(instructionFile) {
		return projection, acpFailure(sessionACPCredentialError, "provider instruction filename is invalid")
	}
	privateInstruction := filepath.Join(privateProfile, instructionFile)
	if err := removeProjectedSessionFile(privateInstruction); err != nil {
		return projection, acpFailure(sessionACPCredentialError, "private provider instructions are unsafe")
	}
	// The box overlays the trusted generated instructions read-only. Keep the underlying
	// session-private path absent so a prior turn cannot author its next trusted frame.
	projection.files = append(projection.files, privateInstruction)
	if err := ensureNoSymlinkPath(sourceProfile); err != nil {
		return projection, acpFailure(sessionACPCredentialError, "source credential account is unsafe")
	}

	live := agent.LiveCredentials()
	if len(live.Artifacts) == 0 || live.Portability == nil {
		return projection, acpFailure(sessionACPCredentialError, "provider has no credential projection")
	}
	seen := make(map[string]bool, len(live.Artifacts))
	primaryProjected := false
	for _, artifact := range live.Artifacts {
		if !validArtifactName(artifact.Name) || artifact.Project == nil || seen[artifact.Name] {
			return projection, acpFailure(sessionACPCredentialError, "provider credential projection is invalid")
		}
		seen[artifact.Name] = true
		sourcePath := filepath.Join(sourceProfile, artifact.Name)
		data, present, err := readCredentialArtifact(sourcePath)
		if err != nil {
			return projection, acpFailure(sessionACPCredentialError, "source credential artifact is unsafe")
		}
		if !present {
			if artifact.Primary {
				return projection, acpFailure(sessionACPCredentialError, "primary credential data is missing")
			}
			continue
		}
		projected, err := artifact.Project(data)
		if err != nil {
			return projection, acpFailure(sessionACPCredentialError, "credential projection failed")
		}
		if projected == nil {
			if artifact.Primary {
				return projection, acpFailure(sessionACPCredentialError, "primary credential data is missing")
			}
			continue
		}
		if len(projected) == 0 || len(projected) > sessionACPArtifactLimit || bytes.IndexByte(projected, 0) >= 0 {
			return projection, acpFailure(sessionACPCredentialError, "projected credential output is invalid")
		}
		destination := filepath.Join(privateProfile, artifact.Name)
		projection.files = append(projection.files, destination)
		if err := writeCredentialArtifact(destination, projected); err != nil {
			return projection, acpFailure(sessionACPCredentialError, "projected credential output is unsafe")
		}
		if artifact.Primary {
			primaryProjected = true
		}
	}
	if !primaryProjected {
		return projection, acpFailure(sessionACPCredentialError, "primary credential data is missing")
	}
	if status := live.Portability(privateProfile, deadline); status != agents.CredentialPortable {
		return projection, acpFailure(sessionACPCredentialError, "credential is not portable through the turn deadline")
	}

	// A target without @account means the source provider default. Bind that same default in the
	// private config without copying the shared defaults file, so the child receives the exact
	// selected account while the command remains the session's exact target string.
	if len(target.Accounts) == 0 {
		defaults := filepath.Join(privateRoot, "defaults")
		projection.files = append(projection.files, defaults)
		if err := writePrivateDefaults(defaults, target.Provider, account); err != nil {
			return projection, acpFailure(sessionACPCredentialError, "private account selection could not be written")
		}
	}
	return projection, nil
}

func (r *sessionTurnRunner) projectSessionConfigFiles(sourceRoot string, projection *sessionACPProjection) error {
	mcpFile := r.sourceCfg.MCPFile
	if mcpFile == "" {
		mcpFile = filepath.Join(sourceRoot, "mcp.json")
	}
	sources := []struct {
		name string
		path string
	}{
		{name: "env", path: filepath.Join(sourceRoot, "env")},
		{name: "mcp.json", path: mcpFile},
		{name: "INSTRUCTIONS.md", path: filepath.Join(sourceRoot, "INSTRUCTIONS.md")},
	}
	for _, source := range sources {
		destination := filepath.Join(projection.privateRoot, source.name)
		if err := removeProjectedSessionFile(destination); err != nil {
			return acpFailure(sessionACPCredentialError, "stale private config is unsafe")
		}
		sourcePath, err := filepath.Abs(filepath.Clean(source.path))
		if err != nil {
			return acpFailure(sessionACPCredentialError, "source private config path is invalid")
		}
		if sameOrBelow(sourcePath, projection.privateRoot) {
			return acpFailure(sessionACPCredentialError, "source private config overlaps session state")
		}
		if err := ensureNoSymlinkPath(filepath.Dir(sourcePath)); err != nil {
			return acpFailure(sessionACPCredentialError, "source private config path is unsafe")
		}
		data, present, err := readCredentialArtifact(sourcePath)
		if err != nil {
			return acpFailure(sessionACPCredentialError, "source private config is unsafe")
		}
		if !present {
			continue
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return acpFailure(sessionACPCredentialError, "source private config is invalid")
		}
		projection.files = append(projection.files, destination)
		if err := writeCredentialArtifact(destination, data); err != nil {
			return acpFailure(sessionACPCredentialError, "private config projection failed")
		}
	}
	return nil
}

// CleanupSession removes transient runtime state left by interrupted processes. Agent-owned
// session history and Compose volumes are deliberately preserved.
func (r *sessionTurnRunner) CleanupSession(ctx context.Context, bound session.Session) error {
	return errors.Join(
		r.cleanupKnownSessionRuntime(ctx, bound),
		r.removeTurnBox(sessionWarmRunID(bound.ID)),
	)
}

func (r *sessionTurnRunner) cleanupKnownSessionRuntime(ctx context.Context, bound session.Session) error {
	return errors.Join(
		r.evictWarmExecution(bound.ID),
		r.cleanupSessionCredentials(bound),
		r.stopSessionServices(ctx, bound),
	)
}

func (r *sessionTurnRunner) cleanupSessionCredentials(bound session.Session) error {
	if r == nil || r.sourceCfg == nil || r.stateRoot == "" || !validSessionPathComponent(bound.ID) {
		return acpFailure(sessionACPCleanupError, "projected credential cleanup is unavailable")
	}
	root, err := filepath.EvalSymlinks(r.stateRoot)
	if err != nil || !filepath.IsAbs(root) {
		return acpFailure(sessionACPCleanupError, "private credential root is invalid")
	}
	privateRoot := filepath.Join(root, "acp", bound.ID)
	if _, err := os.Lstat(privateRoot); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return acpFailure(sessionACPCleanupError, "private credential root inspection failed")
	}
	target, err := agents.ParseTarget(bound.Target)
	if err != nil || len(target.Accounts) > 1 {
		return acpFailure(sessionACPCleanupError, "session target is invalid")
	}
	agent, ok := agents.Get(target.Provider)
	if !ok {
		return acpFailure(sessionACPCleanupError, "session target provider is unavailable")
	}
	account := target.Account()
	if account == "" {
		account = r.sourceCfg.DefaultProfileOf(target.Provider)
	}
	if !validSessionPathComponent(account) {
		return acpFailure(sessionACPCleanupError, "session credential account is invalid")
	}
	profile := filepath.Join(privateRoot, target.Provider, "profiles", account)
	if err := ensureNoSymlinkPath(profile); err != nil {
		return acpFailure(sessionACPCleanupError, "private credential account is unsafe")
	}
	var paths []string
	seen := make(map[string]bool)
	for _, artifact := range agent.LiveCredentials().Artifacts {
		if !validArtifactName(artifact.Name) || seen[artifact.Name] {
			return acpFailure(sessionACPCleanupError, "provider credential projection is invalid")
		}
		seen[artifact.Name] = true
		paths = append(paths, filepath.Join(profile, artifact.Name))
	}
	if instructionFile := agent.InstructionFile(); validArtifactName(instructionFile) {
		paths = append(paths, filepath.Join(profile, instructionFile))
	}
	for _, name := range []string{"defaults", "env", "mcp.json", "INSTRUCTIONS.md"} {
		paths = append(paths, filepath.Join(privateRoot, name))
	}
	if err := (&sessionACPProjection{files: paths}).remove(); err != nil {
		return err
	}
	return nil
}

func (r *sessionTurnRunner) stopSessionServices(parent context.Context, bound session.Session) error {
	if bound.State == session.SessionDiscarded {
		return nil
	}
	if r == nil || r.rt.Name == "" || bound.Workspace == "" || bound.Repository == "" {
		return acpFailure(sessionACPCleanupError, "session services cleanup is unavailable")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, sessionACPCleanupTimeout)
	defer cancel()
	if err := box.StopSessionServices(ctx, r.rt, bound.Workspace, bound.Repository); err != nil {
		return acpFailure(sessionACPCleanupError, "session services cleanup failed")
	}
	return nil
}

func validSessionPathComponent(value string) bool {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value || strings.HasPrefix(value, "-") {
		return false
	}
	return !strings.ContainsAny(value, "/\\\x00\r\n")
}

func validArtifactName(value string) bool {
	return validSessionPathComponent(value) && value != "defaults"
}

func sameOrBelow(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func ensurePrivateDirectory(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("private directory must be absolute")
	}
	if err := ensureSessionAncestors(path); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func ensureNoSymlinkPath(path string) error {
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				current = filepath.Dir(current)
				if current == filepath.Dir(current) {
					break
				}
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("path is not a real directory")
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return nil
}

func readCredentialArtifact(path string) ([]byte, bool, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, errors.New("credential artifact is not regular")
	}
	data, err := io.ReadAll(io.LimitReader(file, sessionACPArtifactLimit+1))
	if err != nil || len(data) > sessionACPArtifactLimit {
		return nil, false, errors.New("credential artifact is too large")
	}
	return data, true, nil
}

func writeCredentialArtifact(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return errors.New("existing credential artifact is unsafe")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func writePrivateDefaults(path, provider, account string) error {
	if !validSessionPathComponent(provider) || !validSessionPathComponent(account) {
		return errors.New("invalid default account")
	}
	return writePrivateConfig(path, []byte(provider+"="+account+"\n"))
}

func writePrivateConfig(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func (r *sessionTurnRunner) startChild(ctx context.Context, bound session.Session, leased session.Turn, privateRoot string) (*sessionACPProcess, error) {
	return r.startChildWithRunID(ctx, bound, sessionTurnRunID(bound.ID, leased.ID), privateRoot)
}

func (r *sessionTurnRunner) startChildWithRunID(ctx context.Context, bound session.Session, runID, privateRoot string) (*sessionACPProcess, error) {
	executable := r.executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil || executable == "" {
			return nil, acpFailure(sessionACPProcessError, "Coop executable is unavailable")
		}
	}
	if !filepath.IsAbs(bound.Repository) || !filepath.IsAbs(bound.Workspace) ||
		bound.Workspace != forkWorkspace(bound.Repository, bound.ForkName) ||
		!validExistingForkName(bound.ForkName) || bound.Target == "" {
		return nil, acpFailure(sessionACPProcessError, "bound fork identity is invalid")
	}
	if !validSessionRunID(runID) {
		return nil, acpFailure(sessionACPProcessError, "session run identity is invalid")
	}
	env := sessionACPChildEnvironment(
		bound.Repository, bound.Companions, privateRoot, runID,
		r.sourceCfg, r.rt.Name,
	)
	cmd := r.command(executable, "fork", bound.ForkName, "acp", bound.Target)
	if cmd == nil {
		return nil, acpFailure(sessionACPProcessError, "Coop child could not be constructed")
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := ctx.Err(); err != nil {
		return nil, classifyContextFailure(err)
	}
	process, err := startSessionACPProcess(cmd)
	if process != nil {
		process.runID = runID
	}
	return process, err
}

func sessionACPChildEnvironment(
	repo string,
	companions []session.CompanionRepository,
	privateRoot, runID string,
	cfg *config.Config,
	runtimeName string,
) []string {
	blocked := map[string]bool{}
	for _, name := range agents.Names() {
		if agent, ok := agents.Get(name); ok {
			for _, key := range agent.CredentialEnvKeys() {
				blocked[key] = true
			}
		}
	}
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if blocked[key] || (strings.HasPrefix(key, "COOP_") && !strings.HasPrefix(key, "COOP_TEST_SESSION_")) {
			continue
		}
		env = append(env, item)
	}
	env = append(env,
		"COOP_REPO="+repo,
		"COOP_CONFIG_DIR="+privateRoot,
		"COOP_CONF="+filepath.Join(privateRoot, ".coop-conf-disabled"),
		"COOP_HOMES=1",
		"COOP_SESSION_RUN_ID="+runID,
	)
	if len(companions) > 0 {
		// CompanionRepository contains strings only, so this encoding cannot fail.
		data, _ := json.Marshal(companions)
		env = append(env, "COOP_SESSION_COMPANIONS="+string(data))
	}
	if runtimeName != "" {
		env = append(env, "COOP_RUNTIME="+runtimeName)
	}
	if cfg != nil {
		stringsByKey := map[string]string{
			"COOP_BASE_IMAGE":   cfg.BaseImage,
			"COOP_WORKDIR":      cfg.Workdir,
			"COOP_HOME_IN_BOX":  cfg.HomeInBox,
			"COOP_IMAGE":        cfg.ImageOverride,
			"COOP_SERVICES_NET": cfg.ServicesNet,
			"COOP_MEMORY":       cfg.Memory,
			"COOP_CPUS":         cfg.CPUs,
			"COOP_PIDS":         cfg.Pids,
			"COOP_EGRESS":       cfg.Egress,
		}
		for _, key := range []string{
			"COOP_BASE_IMAGE", "COOP_WORKDIR", "COOP_HOME_IN_BOX", "COOP_IMAGE",
			"COOP_SERVICES_NET", "COOP_MEMORY", "COOP_CPUS", "COOP_PIDS", "COOP_EGRESS",
		} {
			if cfg.Explicit(key) {
				env = append(env, key+"="+stringsByKey[key])
			}
		}
		boolsByKey := map[string]bool{
			"COOP_NETWORK":           cfg.Network,
			"COOP_AUTO_UP":           cfg.AutoUp,
			"COOP_CACHE":             cfg.Cache,
			"COOP_NO_NEW_PRIVILEGES": cfg.NoNewPrivileges,
		}
		for _, key := range []string{"COOP_NETWORK", "COOP_AUTO_UP", "COOP_CACHE", "COOP_NO_NEW_PRIVILEGES"} {
			if cfg.Explicit(key) {
				env = append(env, key+"="+strconv.FormatBool(boolsByKey[key]))
			}
		}
	}
	return env
}

func sessionTurnRunID(sessionID, turnID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + turnID))
	return "session-" + fmt.Sprintf("%x", sum[:12])
}

func sessionWarmRunID(sessionID string) string {
	sum := sha256.Sum256([]byte("warm\x00" + sessionID))
	return "session-" + fmt.Sprintf("%x", sum[:12])
}

func validSessionRunID(value string) bool {
	if !strings.HasPrefix(value, "session-") || len(value) != len("session-")+24 {
		return false
	}
	for _, r := range value[len("session-"):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func sessionRunIDFromEnv() string {
	value := os.Getenv("COOP_SESSION_RUN_ID")
	if validSessionRunID(value) {
		return value
	}
	return ""
}

type sessionACPProcess struct {
	cmd                    *exec.Cmd
	stdin                  io.WriteCloser
	stdout                 io.ReadCloser
	wait                   chan error
	readStop               chan struct{}
	frames                 chan sessionACPFrame
	closeOnce              sync.Once
	stopOnce               sync.Once
	stopErr                error
	runID                  string
	nextID                 int64
	initialized            bool
	nativeSessionID        string
	imageCapable           bool
	embeddedContextCapable bool
	exited                 atomic.Bool
}

func startSessionACPProcess(cmd *exec.Cmd) (*sessionACPProcess, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, acpFailure(sessionACPProcessError, "ACP stdin pipe could not be created")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, acpFailure(sessionACPProcessError, "ACP stdout pipe could not be created")
	}
	process := &sessionACPProcess{
		cmd: cmd, stdin: stdin, stdout: stdout,
		wait: make(chan error, 1), readStop: make(chan struct{}),
		frames: make(chan sessionACPFrame, 1),
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		process.closePipes()
		return nil, acpFailure(sessionACPProcessError, "ACP child could not be started")
	}
	go func() {
		err := cmd.Wait()
		process.exited.Store(true)
		process.wait <- err
	}()
	go readSessionACPFrames(process.stdout, process.readStop, process.frames)
	return process, nil
}

func (p *sessionACPProcess) closePipes() {
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()
		_ = p.stdout.Close()
	})
}

func (p *sessionACPProcess) stop() error {
	if p == nil {
		return nil
	}
	p.stopOnce.Do(func() { p.stopErr = p.stopProcess() })
	return p.stopErr
}

func (p *sessionACPProcess) stopProcess() error {
	close(p.readStop)
	_ = p.stdin.Close()
	_ = p.stdout.Close()

	if p.cmd.Process != nil {
		if _, done := pollWait(p.wait); !done {
			signalSessionACPGroup(p.cmd.Process.Pid, syscall.SIGTERM)
			if !waitSessionACP(p.wait, sessionACPTermGrace) {
				signalSessionACPGroup(p.cmd.Process.Pid, syscall.SIGKILL)
				if !waitSessionACP(p.wait, sessionACPKillGrace) {
					return acpFailure(sessionACPCleanupError, "ACP child did not stop")
				}
			}
		}
		if sessionACPGroupAlive(p.cmd.Process.Pid) {
			signalSessionACPGroup(p.cmd.Process.Pid, syscall.SIGKILL)
			if !waitSessionACPGroupGone(p.cmd.Process.Pid, sessionACPKillGrace) {
				return acpFailure(sessionACPCleanupError, "ACP process group survived cleanup")
			}
		}
	}
	return nil
}

func pollWait(wait <-chan error) (error, bool) {
	select {
	case err := <-wait:
		return err, true
	default:
		return nil, false
	}
}

func waitSessionACP(wait <-chan error, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-wait:
		return true
	case <-timer.C:
		return false
	}
}

func signalSessionACPGroup(pid int, signal syscall.Signal) {
	if syscall.Kill(-pid, signal) != nil {
		_ = syscall.Kill(pid, signal)
	}
}

func sessionACPGroupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitSessionACPGroupGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for sessionACPGroupAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return !sessionACPGroupAlive(pid)
}

type sessionACPFrame struct {
	line []byte
	err  error
}

func (r *sessionTurnRunner) runACP(ctx context.Context, process *sessionACPProcess, bound session.Session, leased session.Turn) (string, []session.OutputArtifact, error) {
	if process == nil || process.frames == nil {
		return "", nil, acpFailure(sessionACPProcessError, "ACP child is unavailable")
	}
	frames := process.frames
	var transcriptBytes int
	var assistant []byte
	var outputArtifacts []session.OutputArtifact
	collectAssistant := false
	next := func() json.RawMessage {
		process.nextID++
		return json.RawMessage(strconv.FormatInt(process.nextID, 10))
	}
	writeRequest := func(id json.RawMessage, method string, params any) error {
		line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err != nil || len(line)+1 > sessionACPFrameLimit {
			return acpFailure(sessionACPProtocolError, "outgoing ACP frame exceeded its bound")
		}
		line = append(line, '\n')
		return writeSessionACP(ctx, process.stdin, line)
	}
	handle := func(frame []byte, expectedSession string) (json.RawMessage, string, error) {
		var envelope struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil || envelope.JSONRPC != "2.0" {
			return nil, "", acpFailure(sessionACPProtocolError, "malformed ACP frame")
		}
		imageFrame := false
		if envelope.Method != "" {
			if envelope.Method == "session/update" {
				if collectAssistant {
					var err error
					imageFrame, err = accumulateSessionACPUpdateArtifacts(envelope.Params, expectedSession, &assistant, &outputArtifacts)
					if err != nil {
						return nil, "", err
					}
				}
				if imageFrame {
					transcriptBytes += 512
				} else {
					transcriptBytes += len(frame) + 1
				}
				if transcriptBytes > sessionACPTranscriptLimit {
					return nil, "", acpFailure(sessionACPProtocolError, "ACP transcript exceeded its bound")
				}
				return nil, "", nil
			}
			if len(envelope.ID) == 0 || bytes.Equal(bytes.TrimSpace(envelope.ID), []byte("null")) {
				return nil, "", nil
			}
			if envelope.Method == "session/request_permission" {
				var params struct {
					Options []permOption `json:"options"`
				}
				if err := json.Unmarshal(envelope.Params, &params); err != nil {
					return nil, "", acpFailure(sessionACPProtocolError, "malformed permission request")
				}
				outcome := map[string]any{"outcome": "cancelled"}
				if option := chooseSessionACPAllow(params.Options); option != "" {
					outcome = map[string]any{"outcome": "selected", "optionId": option}
				}
				if err := writeSessionACPResponse(ctx, process.stdin, envelope.ID, map[string]any{"outcome": outcome}); err != nil {
					return nil, "", err
				}
				return nil, "", nil
			}
			if err := writeSessionACPError(ctx, process.stdin, envelope.ID, -32601, "no capability"); err != nil {
				return nil, "", err
			}
			return nil, "", nil
		}
		transcriptBytes += len(frame) + 1
		if transcriptBytes > sessionACPTranscriptLimit {
			return nil, "", acpFailure(sessionACPProtocolError, "ACP transcript exceeded its bound")
		}
		return envelope.ID, string(envelope.Result), nil
	}
	waitResponse := func(id json.RawMessage, expectedSession string, cancelPrompt bool) (json.RawMessage, error) {
		for {
			select {
			case <-ctx.Done():
				if cancelPrompt && expectedSession != "" {
					cancelCtx, cancel := context.WithTimeout(context.Background(), sessionACPTermGrace)
					writeErr := writeSessionACPJSON(cancelCtx, process.stdin, map[string]any{
						"jsonrpc": "2.0",
						"method":  "session/cancel",
						"params":  map[string]any{"sessionId": expectedSession},
					})
					cancel()
					if writeErr == nil {
						timer := time.NewTimer(10 * time.Millisecond)
						<-timer.C
					}
				}
				return nil, classifyContextFailure(ctx.Err())
			case frame := <-frames:
				if frame.err != nil {
					if errors.Is(frame.err, io.EOF) {
						return nil, acpFailure(sessionACPProcessError, "ACP child closed before its response")
					}
					return nil, frame.err
				}
				responseID, result, err := handle(frame.line, expectedSession)
				if err != nil {
					return nil, err
				}
				if responseID != nil && bytes.Equal(bytes.TrimSpace(responseID), bytes.TrimSpace(id)) {
					var envelope struct {
						Error json.RawMessage `json:"error"`
					}
					if err := json.Unmarshal(frame.line, &envelope); err != nil {
						return nil, acpFailure(sessionACPProtocolError, "malformed ACP response")
					}
					if len(envelope.Error) != 0 && string(envelope.Error) != "null" {
						return nil, acpFailure(sessionACPProtocolError, "ACP request was rejected")
					}
					return json.RawMessage(result), nil
				}
			}
		}
	}
	request := func(method string, params any, expectedSession string) (json.RawMessage, error) {
		id := next()
		if err := writeRequest(id, method, params); err != nil {
			return nil, err
		}
		return waitResponse(id, expectedSession, false)
	}

	if !process.initialized {
		initializeResult, err := request("initialize", map[string]any{
			"protocolVersion":    1,
			"clientCapabilities": map[string]any{},
		}, "")
		if err != nil {
			return "", nil, err
		}
		var initialized struct {
			ProtocolVersion   int `json:"protocolVersion"`
			AgentCapabilities struct {
				PromptCapabilities struct {
					Image           bool `json:"image"`
					EmbeddedContext bool `json:"embeddedContext"`
				} `json:"promptCapabilities"`
			} `json:"agentCapabilities"`
		}
		if json.Unmarshal(initializeResult, &initialized) != nil || initialized.ProtocolVersion != 1 {
			return "", nil, acpFailure(sessionACPProtocolError, "ACP protocol version is unsupported")
		}
		process.imageCapable = initialized.AgentCapabilities.PromptCapabilities.Image
		process.embeddedContextCapable = initialized.AgentCapabilities.PromptCapabilities.EmbeddedContext
		nativeID := bound.NativeSessionID
		if nativeID == "" {
			result, err := request("session/new", map[string]any{"cwd": bound.Workspace, "mcpServers": []any{}}, "")
			if err != nil {
				return "", nil, err
			}
			var created struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(result, &created) != nil || !validACPSessionID(created.SessionID) {
				return "", nil, acpFailure(sessionACPProtocolError, "session/new returned an invalid session id")
			}
			nativeID = created.SessionID
		} else if !validACPSessionID(nativeID) {
			return "", nil, acpFailure(sessionACPProtocolError, "stored native session id is invalid")
		} else if _, err := request("session/load", map[string]any{"sessionId": nativeID, "cwd": bound.Workspace, "mcpServers": []any{}}, nativeID); err != nil {
			return "", nil, err
		}
		process.nativeSessionID = nativeID
		process.initialized = true
	}
	nativeID := process.nativeSessionID
	if !validACPSessionID(nativeID) || (bound.NativeSessionID != "" && bound.NativeSessionID != nativeID) {
		return "", nil, acpFailure(sessionACPProtocolError, "warm ACP session identity changed")
	}
	if leased.ID == "" {
		return "", nil, nil
	}
	if bound.NativeSessionID == "" {
		ctxBind, cancel := context.WithTimeout(context.Background(), sessionACPCleanupTimeout)
		_, err := r.store.BindNativeSession(ctxBind, bound.ID, nativeID)
		cancel()
		if err != nil {
			return "", nil, err
		}
	}
	checkpointCtx, checkpointCancel := context.WithTimeout(context.Background(), sessionACPCleanupTimeout)
	_, err := r.store.MarkTurnSendIntent(checkpointCtx, bound.ID, leased.ID)
	checkpointCancel()
	if err != nil {
		return "", nil, acpFailure(session.CodeInternal, "turn sent checkpoint failed")
	}
	outputDir, outputRelative, err := prepareSessionOutputDir(bound.Workspace, leased.ID)
	if err != nil {
		return "", nil, acpFailure(sessionACPProtocolError, "turn output directory could not be prepared")
	}
	defer removeSessionOutputDir(outputDir)
	content, err := sessionACPInputContent(leased, process.imageCapable, process.embeddedContextCapable)
	if err != nil {
		return "", nil, err
	}
	content[0]["text"] = fmt.Sprintf("<coop-output>Save only final generated images and charts in %s. Use PNG, JPEG, WebP, or GIF; at most %d files and %d bytes total. Keep source data, virtual environments, caches, and other scratch content outside this directory. Do not put image bytes or data URLs in your reply. Refer to saved filenames in the structured response when the caller requests visuals. Direct image outputs returned by tools are captured in order as generated-1.png (or the matching image extension), generated-2.png, and so on.</coop-output>\n\n%s", outputRelative, session.MaxTurnArtifacts, session.MaxTurnArtifactBytes, leased.Prompt)
	prompt := map[string]any{"sessionId": nativeID, "prompt": content}
	id := next()
	if err := writeRequest(id, "session/prompt", prompt); err != nil {
		return "", nil, err
	}
	checkpointCtx, checkpointCancel = context.WithTimeout(context.Background(), sessionACPCleanupTimeout)
	_, err = r.store.MarkTurnSent(checkpointCtx, bound.ID, leased.ID)
	checkpointCancel()
	if err != nil {
		return "", nil, err
	}
	collectAssistant = true
	result, err := waitResponse(id, nativeID, true)
	if err != nil {
		return "", nil, err
	}
	var promptResult struct {
		StopReason string `json:"stopReason"`
	}
	if json.Unmarshal(result, &promptResult) != nil || !validACPStopReason(promptResult.StopReason) {
		return "", nil, acpFailure(sessionACPProtocolError, "session/prompt returned an invalid stop reason")
	}
	if promptResult.StopReason == "cancelled" || promptResult.StopReason == "error" {
		return "", nil, acpFailure(sessionACPCancelledError, "ACP prompt was cancelled")
	}
	if len(assistant) > sessionACPMessageLimit || !utf8.Valid(assistant) {
		return "", nil, acpFailure(sessionACPProtocolError, "assistant message exceeded its bound")
	}
	payload, err := json.Marshal(map[string]any{"text": string(assistant), "final": true})
	if err != nil || len(payload) > session.MaxEventPayloadBytes {
		return "", nil, acpFailure(sessionACPProtocolError, "assistant message exceeded its durable event bound")
	}
	files, err := collectSessionOutputDir(outputDir)
	if err != nil {
		return "", nil, acpFailure(sessionACPProtocolError, err.Error())
	}
	for _, artifact := range files {
		outputArtifacts = appendOutputArtifact(outputArtifacts, artifact.Name, artifact.MediaType, artifact.Data)
	}
	if len(outputArtifacts) > session.MaxTurnArtifacts {
		return "", nil, acpFailure(sessionACPProtocolError, "turn produced too many output artifacts")
	}
	total := 0
	for _, artifact := range outputArtifacts {
		total += len(artifact.Data)
	}
	if total > session.MaxTurnArtifactBytes {
		return "", nil, acpFailure(sessionACPProtocolError, "turn output artifacts exceed their total bound")
	}
	return string(assistant), outputArtifacts, nil
}

func readSessionACPFrames(reader io.Reader, stop <-chan struct{}, frames chan<- sessionACPFrame) {
	br := bufio.NewReaderSize(reader, sessionACPFrameLimit)
	for {
		line, err := readSessionACPLine(br)
		if len(line) > 0 {
			select {
			case frames <- sessionACPFrame{line: line}:
			case <-stop:
				return
			}
		}
		if err != nil {
			select {
			case frames <- sessionACPFrame{err: err}:
			case <-stop:
			}
			return
		}
	}
}

func readSessionACPLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, prefix, err := reader.ReadLine()
		if len(part) > 0 {
			line = append(line, part...)
			if len(line) > sessionACPFrameLimit {
				return nil, acpFailure(sessionACPProtocolError, "ACP frame exceeded its bound")
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return nil, acpFailure(sessionACPProtocolError, "ACP frame was not newline terminated")
			}
			return line, err
		}
		if !prefix {
			return line, nil
		}
	}
}

func writeSessionACP(ctx context.Context, writer io.Writer, line []byte) error {
	result := make(chan error, 1)
	go func() {
		n, err := writer.Write(line)
		if err == nil && n != len(line) {
			err = io.ErrShortWrite
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			return acpFailure(sessionACPProcessError, "ACP frame could not be written")
		}
		return nil
	case <-ctx.Done():
		return classifyContextFailure(ctx.Err())
	}
}

func writeSessionACPResponse(ctx context.Context, writer io.Writer, id json.RawMessage, result any) error {
	return writeSessionACPJSON(ctx, writer, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeSessionACPError(ctx context.Context, writer io.Writer, id json.RawMessage, code int, message string) error {
	return writeSessionACPJSON(ctx, writer, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func writeSessionACPJSON(ctx context.Context, writer io.Writer, value any) error {
	line, err := json.Marshal(value)
	if err != nil || len(line)+1 > sessionACPFrameLimit {
		return acpFailure(sessionACPProtocolError, "ACP response exceeded its bound")
	}
	return writeSessionACP(ctx, writer, append(line, '\n'))
}

func validACPSessionID(id string) bool {
	return id != "" && len(id) <= session.MaxIDBytes && utf8.ValidString(id) && !strings.ContainsAny(id, "\x00\r\n")
}

func validACPStopReason(reason string) bool {
	switch reason {
	case "end_turn", "max_tokens", "max_turn_requests", "refusal", "cancelled", "error":
		return true
	default:
		return false
	}
}

func chooseSessionACPAllow(options []permOption) string {
	for _, want := range []string{"allow_always", "allow_once"} {
		for _, option := range options {
			if option.Kind == want && option.OptionID != "" && len(option.OptionID) <= session.MaxIDBytes && utf8.ValidString(option.OptionID) {
				return option.OptionID
			}
		}
	}
	return ""
}

func accumulateSessionACPUpdate(raw json.RawMessage, expectedSession string, assistant *[]byte) error {
	_, err := accumulateSessionACPUpdateArtifacts(raw, expectedSession, assistant, nil)
	return err
}

func accumulateSessionACPUpdateArtifacts(raw json.RawMessage, expectedSession string, assistant *[]byte, artifacts *[]session.OutputArtifact) (bool, error) {
	var envelope struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false, acpFailure(sessionACPProtocolError, "malformed session update")
	}
	if envelope.SessionID != "" && envelope.SessionID != expectedSession {
		return false, acpFailure(sessionACPProtocolError, "session update belongs to another session")
	}
	imageFrame, err := collectACPOutputImages(envelope.Update.Content, artifacts)
	if err != nil {
		return false, acpFailure(sessionACPProtocolError, err.Error())
	}
	if envelope.Update.SessionUpdate != "assistant_message_chunk" && envelope.Update.SessionUpdate != "agent_message_chunk" {
		return imageFrame, nil
	}
	var content struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}
	if err := json.Unmarshal(envelope.Update.Content, &content); err != nil {
		return false, acpFailure(sessionACPProtocolError, "malformed assistant message update")
	}
	if content.Type == "image" {
		return imageFrame, nil
	}
	if content.Type != "text" || content.Text == "" {
		return false, nil
	}
	if !utf8.ValidString(content.Text) || len(*assistant)+len(content.Text) > sessionACPMessageLimit {
		return false, acpFailure(sessionACPProtocolError, "assistant message exceeded its bound")
	}
	*assistant = append(*assistant, content.Text...)
	return imageFrame, nil
}

func collectACPOutputImages(raw json.RawMessage, artifacts *[]session.OutputArtifact) (bool, error) {
	if artifacts == nil || len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, nil
	}
	var content any
	if err := json.Unmarshal(raw, &content); err != nil {
		return false, errors.New("malformed session update content")
	}
	nodes := 0
	return visitACPOutputImages(content, artifacts, 0, &nodes)
}

func visitACPOutputImages(value any, artifacts *[]session.OutputArtifact, depth int, nodes *int) (bool, error) {
	*nodes++
	if depth > 32 || *nodes > 4096 {
		return false, errors.New("session update content exceeded its structural bound")
	}
	switch value := value.(type) {
	case []any:
		found := false
		for _, item := range value {
			itemFound, err := visitACPOutputImages(item, artifacts, depth+1, nodes)
			if err != nil {
				return false, err
			}
			found = found || itemFound
		}
		return found, nil
	case map[string]any:
		if value["type"] == "image" {
			data, _ := value["data"].(string)
			mediaType, _ := value["mimeType"].(string)
			artifact, err := decodeACPOutputImage(data, mediaType, len(*artifacts))
			if err != nil {
				return false, err
			}
			*artifacts = appendOutputArtifact(*artifacts, artifact.Name, artifact.MediaType, artifact.Data)
			return true, nil
		}
		if value["type"] == "resource" {
			if resource, ok := value["resource"].(map[string]any); ok {
				blob, _ := resource["blob"].(string)
				mediaType, _ := resource["mimeType"].(string)
				if blob != "" && strings.HasPrefix(mediaType, "image/") {
					artifact, err := decodeACPOutputImage(blob, mediaType, len(*artifacts))
					if err != nil {
						return false, err
					}
					*artifacts = appendOutputArtifact(*artifacts, artifact.Name, artifact.MediaType, artifact.Data)
					return true, nil
				}
			}
		}
		found := false
		for _, item := range value {
			itemFound, err := visitACPOutputImages(item, artifacts, depth+1, nodes)
			if err != nil {
				return false, err
			}
			found = found || itemFound
		}
		return found, nil
	default:
		return false, nil
	}
}
