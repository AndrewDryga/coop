package sessionsvc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/mcp"
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
	// A completed model result is irreversible work, not best-effort cleanup. Under a recovery
	// burst Coop's single SQLite connection can legitimately queue this receipt for a few seconds.
	sessionACPCompletionTimeout = 10 * time.Second
	// sessionRuntimeReapTimeout bounds the docker-facing teardown (box removal, sidecar stop).
	// Separate from the store-call timeout above because `docker rm -f` of a just-killed box on a
	// contended daemon routinely needs more than two seconds — and a slow reap is retried by the
	// per-minute janitor anyway, so the budget only has to be generous enough not to cry wolf.
	sessionRuntimeReapTimeout = 10 * time.Second
	sessionACPWarmLimit       = 20
	sessionACPStderrLimit     = 4 << 10
	sessionACPRejectionLimit  = 300
)

// One initial candidate plus two corrections keeps a broken model from
// burning an unbounded turn while preserving the caller-visible invariant:
// invalid structured output is never a completed turn.
const sessionOutputContractMaxAttempts = session.MaxOutputContractAttempts

const (
	sessionACPProtocolError   session.ErrorCode = "acp_protocol_error"
	sessionACPProcessError    session.ErrorCode = "acp_process_error"
	sessionACPTimeoutError    session.ErrorCode = "acp_timeout"
	sessionACPCancelledError  session.ErrorCode = "acp_cancelled"
	sessionACPCredentialError session.ErrorCode = "credential_projection_error"
	sessionACPCleanupError    session.ErrorCode = "session_cleanup_error"
	sessionACPInvalidTarget   session.ErrorCode = "invalid_session_target"
	sessionACPInvalidTurn     session.ErrorCode = "invalid_leased_turn"
	// sessionACPRateLimited survives to the client only when the whole ladder is cooling: a
	// rung that still has a free sibling is rotated onto it instead, inside the turn.
	sessionACPRateLimited session.ErrorCode = "rate_limited"
)

type sessionACPCommand func(string, ...string) *exec.Cmd

type sessionCancelRequestContextKey struct{}
type sessionWarmIdleTimeoutContextKey struct{}
type sessionTargetLadderContextKey struct{}

type sessionWarmExecution struct {
	bound              session.Session
	child              *sessionACPProcess
	projection         *sessionACPProjection
	credentialDeadline time.Time
	expiresAt          time.Time
	timer              *time.Timer
}

type turnCompletionStore interface {
	CompleteTurn(context.Context, session.CompleteTurnRequest) (session.Turn, error)
}

// sessionTurnRunner owns boxed ACP children. Policy-opted sessions can retain one authenticated
// child across serialized turns; the same Coop fork path still owns box assembly and labels.
type sessionTurnRunner struct {
	sourceCfg  *config.Config
	stateRoot  string
	store      *session.Store
	completion turnCompletionStore
	rt         runtime.Runtime
	executable string
	host       Host
	command    sessionACPCommand
	// readMCPSnapshot is the shared authority boundary; nil uses mcp.ReadValidatedSnapshot.
	// Tests replace it only to make source-retarget timing deterministic.
	readMCPSnapshot func(string) ([]byte, bool, error)
	warmMu          sync.Mutex
	warm            map[string]*sessionWarmExecution
	warming         int
	// Rate-limit cooldowns per session, so a rung limited on one turn is still skipped on the
	// next. Deliberately in memory: the durable Session.Target already says which rung a
	// restarted controller resumes on, and re-probing a cooled rung once costs one turn.
	rotateMu  sync.Mutex
	rotations map[string]*sessionLadder
	// activityClock is the narration recorder's clock; nil is time.Now. Only tests set it, so
	// the alive heartbeat's minute-long window can be asserted without a minute-long test.
	activityClock func() time.Time
}

// sessionLadder is a session's rotation plus the ladder it was built from, so a policy edit
// rebuilds it instead of rotating against rungs that no longer exist.
type sessionLadder struct {
	rendered string
	rotation *ladder.Rotation
}

func newSessionTurnRunner(sourceCfg *config.Config, stateRoot string, store *session.Store, rt runtime.Runtime, executable string, command ...sessionACPCommand) *sessionTurnRunner {
	start := sessionACPCommand(exec.Command)
	if len(command) > 0 && command[0] != nil {
		start = command[0]
	}
	return &sessionTurnRunner{
		sourceCfg: sourceCfg, stateRoot: stateRoot,
		store: store, completion: store, rt: rt, executable: executable, command: start,
		warm:      make(map[string]*sessionWarmExecution),
		rotations: make(map[string]*sessionLadder),
	}
}

// sessionRotation returns the session's ladder rotation, pointed at the rung the session is
// actually on. Session.Target is the durable truth across a restart; the cooldown marks are not,
// so they are carried in memory for as long as the controller lives.
func (r *sessionTurnRunner) sessionRotation(sessionID string, rungs []agents.Target, active string) *ladder.Rotation {
	if len(rungs) < 2 {
		return nil
	}
	rendered := sessionTargetList(rungs)
	r.rotateMu.Lock()
	defer r.rotateMu.Unlock()
	entry := r.rotations[sessionID]
	if entry == nil || entry.rendered != rendered {
		entry = &sessionLadder{rendered: rendered, rotation: ladder.NewRotation(rungs)}
		r.rotations[sessionID] = entry
	}
	entry.rotation.Focus(active)
	return entry.rotation
}

func (r *sessionTurnRunner) forgetRotation(sessionID string) {
	r.rotateMu.Lock()
	delete(r.rotations, sessionID)
	r.rotateMu.Unlock()
}

// sessionLadderIndex is where a rung sits on the ladder, or -1 when the ladder does not hold it.
func sessionLadderIndex(members []string, target string) int {
	for i, member := range members {
		if member == target {
			return i
		}
	}
	return -1
}

// startAtLadderFloor honors a turn's escalation floor before its FIRST delivery: the turn runs no
// lower than rung floor of the policy ladder. Responder re-delivers a corrected turn this way, and
// the rung that produced the answer being corrected is not the rung to correct it on — so a floor
// that cannot be resolved fails the turn instead of quietly answering from below it, which would
// reach the client as an honored escalation. A session already at or above the floor does not
// move: "no lower than rung N" is a floor, not a seat assignment.
func (r *sessionTurnRunner) startAtLadderFloor(
	ctx context.Context, bound *session.Session, leased *session.Turn,
	rungs []agents.Target, rot *ladder.Rotation,
) error {
	floor := leased.MinTargetIndex
	if floor <= 0 {
		return nil
	}
	if rot == nil || floor >= len(rungs) {
		return acpFailure(sessionACPInvalidTarget, fmt.Sprintf(
			"turn requires policy ladder rung %d, and this session's ladder has %d",
			floor, len(rungs),
		))
	}
	if sessionLadderIndex(rot.Members(), bound.Target) >= floor {
		return nil
	}
	current, err := agents.ParseTarget(bound.Target)
	if err != nil {
		return acpFailure(sessionACPInvalidTarget, "session target is unparseable")
	}
	next := rungs[floor]
	rotated, rewound, err := r.store.RotateTurnTarget(
		ctx, bound.ID, leased.ID, bound.Target, next.String(), targetChangeResetsNativeSession(current, next),
	)
	if err != nil {
		return err
	}
	*bound, *leased = rotated, rewound
	rot.Focus(next.String())
	return nil
}

// startAtLadderBeginning applies an explicit one-turn failback before the
// ordinary floor logic. A zero floor cannot do this: it is intentionally
// absent and an ordinary turn inherits the session's durable current rung.
func (r *sessionTurnRunner) startAtLadderBeginning(
	ctx context.Context, bound *session.Session, leased *session.Turn,
	rungs []agents.Target, rot *ladder.Rotation,
) error {
	if !leased.RewindTarget {
		return nil
	}
	if len(rungs) == 0 {
		return acpFailure(sessionACPInvalidTarget,
			"turn requires the first policy ladder rung, and this session has no ladder")
	}
	next := rungs[0]
	if bound.Target == next.String() {
		return nil
	}
	if rot == nil {
		return acpFailure(sessionACPInvalidTarget,
			"turn requires the first policy ladder rung, and this session's target is not on it")
	}
	current, err := agents.ParseTarget(bound.Target)
	if err != nil {
		return acpFailure(sessionACPInvalidTarget, "session target is unparseable")
	}
	rotated, rewound, err := r.store.RotateTurnTarget(
		ctx, bound.ID, leased.ID, bound.Target, next.String(), targetChangeResetsNativeSession(current, next),
	)
	if err != nil {
		return err
	}
	*bound, *leased = rotated, rewound
	rot.Focus(next.String())
	return nil
}

// sessionFloorLimitedUntil is the soonest cooldown to expire among the rungs a floored turn may
// still use. Reaching it means every one of them is cooling — the ladder wraps below the floor
// only when none is free — and the rung just marked is itself at or above the floor with a
// freshly written reset, so the minimum is always a real time.
func sessionFloorLimitedUntil(rot *ladder.Rotation, floor int) time.Time {
	var soonest time.Time
	for _, member := range rot.Members()[floor:] {
		until := rot.LimitedUntil(member)
		if until.IsZero() {
			continue
		}
		if soonest.IsZero() || until.Before(soonest) {
			soonest = until
		}
	}
	return soonest
}

// rotateOnLimit moves the session onto another rung after the active one was rate limited, and
// reports whether the turn should be retried. Only a proven rate limit rotates: an expired or
// revoked credential looks like a failure, not a limit, and must surface so a human fixes it
// instead of having the ladder quietly paper over it.
//
// bound and leased are updated in place to the rung and rewound turn the retry runs on. Each
// rotation marks a rung cooling, so the caller's loop turns over at most once per rung before
// this reports that none is free. backoffs counts the limits this turn has hit, so the narration
// can number them; only a proven limit advances it. floor is the turn's escalation floor, which
// no rotation may cross.
func (r *sessionTurnRunner) rotateOnLimit(
	ctx context.Context, bound *session.Session, leased *session.Turn, rot *ladder.Rotation,
	cause error, backoffs *int, floor int,
) (bool, error) {
	var failure *sessionACPFailure
	if rot == nil || !errors.As(cause, &failure) || failure.code != sessionACPRateLimited {
		return false, nil
	}
	*backoffs++
	now := time.Now()
	marked := rot.Active().String()
	sleep, until := rot.OnLimit(failure.resetAt, 0, now)
	// Read the cooldown back off the rung the ladder just marked rather than reusing the
	// provider's claim: a limit with no reset, or one already in the past, is normalized into the
	// ladder's own bounded backoff, and the client timing its next poll needs the wait that
	// actually applies rather than the zero the provider sent.
	backoff := providerBackoff{
		attempt: *backoffs, target: bound.Target,
		resetAt: failure.resetAt, retryAfter: rot.LimitedUntil(marked).Sub(now),
	}
	if sleep > 0 {
		// Every rung is cooling. Unlike an editor session, a queued turn does not wait it out:
		// the client owns retry and its own backoff, and holding the turn only burns its
		// deadline before failing anyway. Hand back the soonest reset so it can time that retry.
		backoff.allLimitedUntil = until
		r.narrateProviderBackoff(ctx, bound.ID, leased.ID, backoff)
		return false, acpFailure(sessionACPRateLimited,
			"every target in the policy ladder is rate limited until "+until.UTC().Format(time.RFC3339))
	}
	next := rot.Active()
	// The ladder rotates in a circle, so an exhausted top wraps back onto the rung a floored turn
	// was told to start above. Delivering there would answer from the model the escalation existed
	// to replace, and would reach the client looking exactly like an honored escalation. For this
	// turn that wrap is "nothing free": fail on the code the client already retries against, and
	// hand back the soonest reset among the rungs it may still use.
	if floor > 0 && sessionLadderIndex(rot.Members(), next.String()) < floor {
		backoff.allLimitedUntil = sessionFloorLimitedUntil(rot, floor)
		r.narrateProviderBackoff(ctx, bound.ID, leased.ID, backoff)
		return false, acpFailure(sessionACPRateLimited, fmt.Sprintf(
			"every target at or above policy ladder rung %d is rate limited until %s",
			floor, backoff.allLimitedUntil.UTC().Format(time.RFC3339),
		))
	}
	current, err := agents.ParseTarget(bound.Target)
	if err != nil {
		return false, acpFailure(sessionACPInvalidTarget, "session target is unparseable")
	}
	backoff.nextTarget = next.String()
	r.narrateProviderBackoff(ctx, bound.ID, leased.ID, backoff)
	rotated, rewound, err := r.store.RotateTurnTarget(
		ctx, bound.ID, leased.ID, bound.Target, next.String(), targetChangeResetsNativeSession(current, next),
	)
	if err != nil {
		return false, err
	}
	*bound, *leased = rotated, rewound
	return true, nil
}

// Native provider sessions live inside one credential's profile directory. A model or effort
// change on the same credential can resume its transcript; another credential cannot see it.
func targetChangeResetsNativeSession(current, next agents.Target) bool {
	return current.Provider != next.Provider || current.Account() != next.Account()
}

// providerBackoff is one rate-limit decision in the shape the event carries:
// which rung the provider limited, how long it is out, what the turn did next,
// and which backoff of this turn it is.
type providerBackoff struct {
	attempt         int
	target          string
	nextTarget      string
	retryAfter      time.Duration
	resetAt         time.Time
	allLimitedUntil time.Time
}

// narrateProviderBackoff makes a rate-limit decision audible on the event
// stream. Before it, a throttled turn was silent between attempts: a client
// watching events could not tell a model waiting out a 429 from a dead
// transport, and Responder cancelled crawling turns on exactly that ambiguity
// (2026-08-15). One event per proven limit, which the ladder bounds to one per
// rung — a heartbeat a client can count, never a poll-rate ticker. Narration
// failure is never turn failure — the append error is discarded, matching the
// activity recorder's rule — and the write survives a turn already at its
// deadline the same way cleanup does.
func (r *sessionTurnRunner) narrateProviderBackoff(
	ctx context.Context, sessionID, turnID string, backoff providerBackoff,
) {
	payload := map[string]any{"attempt": backoff.attempt, "target": backoff.target}
	if backoff.nextTarget != "" {
		payload["next_target"] = backoff.nextTarget
	}
	if backoff.retryAfter > 0 {
		payload["retry_after_seconds"] = int(backoff.retryAfter.Round(time.Second) / time.Second)
	}
	if !backoff.resetAt.IsZero() {
		payload["reset_at"] = backoff.resetAt.UTC().Format(time.RFC3339)
	}
	if !backoff.allLimitedUntil.IsZero() {
		payload["all_limited_until"] = backoff.allLimitedUntil.UTC().Format(time.RFC3339)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	appendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionACPCleanupTimeout)
	defer cancel()
	_, _ = r.store.AppendEvent(appendCtx, session.AppendEventRequest{
		SessionID: sessionID,
		TurnID:    turnID,
		Type:      session.EventProviderBackoff,
		Version:   1,
		Payload:   encoded,
	})
}

// Run executes exactly one turn returned by Store.LeaseNextTurn. It never leases another turn.
func (r *sessionTurnRunner) Run(ctx context.Context, bound session.Session, leased session.Turn) (result session.Turn, runErr error) {
	var execution *sessionWarmExecution
	var assistant string
	usage := leased.Usage
	var candidateUsage session.Usage
	var outputArtifacts []session.OutputArtifact
	protocolComplete := false
	candidatePublished := false
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

		var cleanupCause error
		for _, err := range cleanup {
			if err != nil {
				cleanupFailed = true
				cleanupCause = errors.Join(cleanupCause, err)
			}
		}
		if cleanupFailed {
			if (protocolComplete || candidatePublished) && baseErr == nil {
				// The answer outranks the janitorial proof. Failing here converted
				// finished multi-minute investigations into errors over a slow
				// `docker rm`, and bought nothing: the per-minute idle-runtime
				// sweep retries exactly this teardown, and the projection sits in
				// the owner-private state root either way until it does. The
				// failure still surfaces — loudly, with its cause — where an
				// operator reads, instead of destroying what the model produced.
				state := "completed"
				if candidatePublished {
					state = "published a semantic candidate"
				}
				r.host.warnf("turn %s %s but its runtime cleanup failed; the janitor retries it: %s",
					leased.ID, state, sessionACPBoundedDetail("cause", cleanupCause.Error()))
			} else {
				if baseErr == nil {
					baseErr = acpFailure(
						sessionACPCleanupError,
						sessionACPBoundedDetail("turn cleanup failed", cleanupCause.Error()),
					)
				}
				baseErr = errors.Join(baseErr, cleanupCause)
			}
		}
		if protocolComplete && baseErr == nil {
			completed, err := r.completeTurn(bound, leased, assistant, outputArtifacts, usage)
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
	semanticResume := leased.OutputContract != nil && leased.OutputContract.RequireSemanticValidation &&
		leased.ValidationAttempt > 0 && leased.State == session.TurnRunning && leased.SendState == session.SendStateSent
	if bound.ID == "" || leased.ID == "" || bound.ID != leased.SessionID ||
		(leased.State != session.TurnStarting && leased.State != session.TurnRunning) ||
		(leased.SendState != session.SendStateNone && !semanticResume) {
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

	rungs, _ := ctx.Value(sessionTargetLadderContextKey{}).([]agents.Target)
	rot := r.sessionRotation(bound.ID, rungs, bound.Target)
	if err := r.startAtLadderBeginning(ctx, &bound, &leased, rungs, rot); err != nil {
		runErr = err
		return result, runErr
	}
	if err := r.startAtLadderFloor(ctx, &bound, &leased, rungs, rot); err != nil {
		runErr = err
		return result, runErr
	}
	floor := leased.MinTargetIndex
	// Counts the rate limits this turn has hit, so its narration numbers them. Per Run and not
	// per runner: a turn re-leased after a controller restart re-probes the ladder from scratch,
	// and numbering that second pass on from the first would claim a continuity it does not have.
	backoffs := 0
	validator, err := session.CompileOutputContract(leased.OutputContract)
	if err != nil {
		runErr = acpFailure(session.CodeOutputContractFailed, "the durable output contract could not be compiled")
		return result, runErr
	}
	// Schema repair happens inside this runner invocation. Semantic repair is a
	// caller-owned round resumed from durable ValidationAttempt. Sharing one
	// counter meant malformed JSON spent a semantic candidate before the caller
	// had inspected any candidate at all.
	schemaAttempt := 0
	semanticAttempt := leased.ValidationAttempt + 1
	prompt := leased.Prompt
	checkpointSend := true
	if leased.OutputContract != nil {
		prompt = sessionOutputContractInitialPrompt(leased.Prompt, leased.OutputContract)
	}
	if semanticResume {
		prompt = sessionOutputContractRepairPrompt(
			leased.OutputContract, semanticAttempt, errors.New(leased.ValidationError),
		)
		checkpointSend = false
	}

	for {
		target, err := agents.ParseTarget(bound.Target)
		if err != nil || len(target.Accounts) > 1 {
			runErr = acpFailure(sessionACPInvalidTarget, "session target must be one explicit provider and credential")
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

		assistant, outputArtifacts, candidateUsage, err = r.runACP(
			ctx, execution.child, bound, leased, prompt, checkpointSend,
		)
		if err == nil {
			usage = addSessionUsage(usage, candidateUsage)
			if bound.NativeSessionID == "" {
				bound.NativeSessionID = execution.child.nativeSessionID
			}
			if validator == nil {
				break
			}
			schemaAttempt++
			validationErr := validator.Validate([]byte(assistant))
			if validationErr == nil {
				if leased.OutputContract.RequireSemanticValidation {
					staged, stageErr := r.stageTurnCandidate(
						bound, leased, assistant, outputArtifacts, usage, semanticAttempt,
					)
					if stageErr != nil {
						runErr = stageErr
						return result, runErr
					}
					candidatePublished = true
					result = staged
					return result, nil
				}
				break
			}
			r.recordOutputContractRejection(ctx, bound.ID, leased.ID, leased.OutputContract.SHA256, schemaAttempt, validationErr)
			if schemaAttempt >= sessionOutputContractMaxAttempts {
				runErr = acpFailure(
					session.CodeOutputContractFailed,
					fmt.Sprintf("provider returned invalid structured output after %d attempts", schemaAttempt),
				)
				return result, runErr
			}
			prompt = sessionOutputContractRepairPrompt(leased.OutputContract, schemaAttempt+1, validationErr)
			checkpointSend = false
			continue
		}
		retry, rotateErr := r.rotateOnLimit(ctx, &bound, &leased, rot, err, &backoffs, floor)
		if rotateErr != nil {
			runErr = rotateErr
			return result, runErr
		}
		if !retry {
			runErr = err
			return result, runErr
		}
		// The next rung needs its own credential projection and box, and the rotated bound no
		// longer matches this execution, so it cannot be reused or parked.
		if cleanupErr := r.cleanupWarmExecution(execution); cleanupErr != nil {
			runErr = errors.Join(err, cleanupErr)
			return result, runErr
		}
		execution = nil
		// Rotation rewinds the durable send checkpoint. A new provider cannot
		// repair against another provider's native transcript, so regenerate from
		// the admitted prompt and exact contract instead of sending only the last
		// correction request.
		prompt = leased.Prompt
		if leased.OutputContract != nil {
			prompt = sessionOutputContractInitialPrompt(leased.Prompt, leased.OutputContract)
		}
		checkpointSend = true
	}
	protocolComplete = true
	return result, nil
}

func addSessionUsage(total, candidate session.Usage) session.Usage {
	total.InputTokens += candidate.InputTokens
	total.CachedInputTokens += candidate.CachedInputTokens
	total.OutputTokens += candidate.OutputTokens
	total.ReasoningTokens += candidate.ReasoningTokens
	// ACP reports cost as a cumulative native-session value. The last observed
	// value supersedes the earlier one; adding it would double-charge repairs.
	if candidate.CostRecorded {
		total.CostUSD = candidate.CostUSD
		total.CostRecorded = true
	}
	return total
}

func sessionOutputContractInitialPrompt(prompt string, contract *session.OutputContract) string {
	if contract == nil {
		return prompt
	}
	return fmt.Sprintf(`%s

<coop-output-contract sha256="%s">
Your final assistant response must be exactly one JSON value that matches this schema.
Before ending the response:
1. Save the exact bytes inside the json-schema element to /tmp/coop-output-contract.schema.json.
2. Save your final candidate to /tmp/coop-output-contract.candidate.json.
3. Run: jv --assert-format --output detailed /tmp/coop-output-contract.schema.json /tmp/coop-output-contract.candidate.json
4. Fix every error and run the command again. Return the candidate only after jv exits successfully.
Do not wrap the candidate in Markdown.
Coop independently validates the final bytes and rejects an invalid candidate.

<json-schema>%s</json-schema>
</coop-output-contract>`, prompt, contract.SHA256, contract.JSONSchema)
}

func sessionOutputContractRepairPrompt(contract *session.OutputContract, attempt int, validationErr error) string {
	detail := sessionACPBoundedDetail("validation failed", validationErr.Error())
	return fmt.Sprintf(`Your previous final response was rejected by output contract %s.
%s
Save the exact bytes inside the json-schema element to /tmp/coop-output-contract.schema.json.
Write the corrected replacement to /tmp/coop-output-contract.candidate.json, then run:
jv --assert-format --output detailed /tmp/coop-output-contract.schema.json /tmp/coop-output-contract.candidate.json
Fix every error and rerun that command. Return the candidate only after jv exits successfully.
Return exactly one JSON value. Do not include explanation or Markdown.
This is correction attempt %d of %d.

<json-schema>%s</json-schema>`, contract.SHA256, detail, attempt, sessionOutputContractMaxAttempts, contract.JSONSchema)
}

func (r *sessionTurnRunner) recordOutputContractRejection(
	ctx context.Context,
	sessionID, turnID, digest string,
	attempt int,
	validationErr error,
) {
	if r.store == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"attempt": attempt,
		"sha256":  digest,
		"error":   sessionACPBoundedDetail("validation failed", validationErr.Error()),
	})
	if err != nil {
		return
	}
	appendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionACPCleanupTimeout)
	defer cancel()
	_, _ = r.store.AppendEvent(appendCtx, session.AppendEventRequest{
		SessionID: sessionID,
		TurnID:    turnID,
		Type:      session.EventOutputContractRejected,
		Version:   1,
		Payload:   payload,
	})
}

type sessionACPFailure struct {
	code   session.ErrorCode
	detail string
	// resetAt is set only on sessionACPRateLimited, when the provider said when it frees up.
	// Zero means "limited, but it did not say" — the ladder falls back to bounded backoff.
	resetAt time.Time
}

func (e *sessionACPFailure) Error() string { return string(e.code) + ": " + e.detail }

// acpUsage is what an adapter reports a turn cost.
//
// Both spellings, because the adapters and the raw provider streams disagree.
// claude-agent-acp and codex-acp answer in camelCase — inputTokens,
// cachedReadTokens, thoughtTokens — while Codex's own stream-json uses
// input_tokens and reasoning_output_tokens. The first version of this parsed
// only the stream-json names against an ACP result, so it matched nothing and
// recorded four zeros for every turn.
//
// Cache reads and cache writes sum into one cached figure: this feeds
// session.Usage, which separates cached input from fresh input because
// providers price those differently, and does not split further than that.
type acpUsage struct {
	InputTokens       int `json:"inputTokens"`
	InputTokensSnake  int `json:"input_tokens"`
	OutputTokens      int `json:"outputTokens"`
	OutputTokensSnake int `json:"output_tokens"`
	CachedRead        int `json:"cachedReadTokens"`
	CachedWrite       int `json:"cachedWriteTokens"`
	CachedSnake       int `json:"cached_input_tokens"`
	Thought           int `json:"thoughtTokens"`
	ReasoningSnake    int `json:"reasoning_output_tokens"`
}

func (u *acpUsage) session() session.Usage {
	if u == nil {
		return session.Usage{}
	}
	return session.Usage{
		InputTokens:       max(u.InputTokens, u.InputTokensSnake),
		OutputTokens:      max(u.OutputTokens, u.OutputTokensSnake),
		CachedInputTokens: max(u.CachedRead+u.CachedWrite, u.CachedSnake),
		ReasoningTokens:   max(u.Thought, u.ReasoningSnake),
	}
}

// sessionACPReportedCost reads ACP's cumulative session cost update. Cost is
// optional and separate from PromptResponse.usage in the protocol; Claude's
// adapter reports it here while Codex currently reports token counts only.
func sessionACPReportedCost(raw json.RawMessage) (float64, bool) {
	var params struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Cost          *struct {
				Amount   float64 `json:"amount"`
				Currency string  `json:"currency"`
			} `json:"cost"`
		} `json:"update"`
	}
	if json.Unmarshal(raw, &params) != nil || params.Update.SessionUpdate != "usage_update" ||
		params.Update.Cost == nil || params.Update.Cost.Currency != "USD" ||
		params.Update.Cost.Amount < 0 || math.IsNaN(params.Update.Cost.Amount) ||
		math.IsInf(params.Update.Cost.Amount, 0) {
		return 0, false
	}
	return params.Update.Cost.Amount, true
}

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

func (r *sessionTurnRunner) completeTurn(bound session.Session, leased session.Turn, assistant string, artifacts []session.OutputArtifact, usage session.Usage) (session.Turn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sessionACPCompletionTimeout)
	defer cancel()
	cumulativeCost, costRecorded := usage.CostUSD, usage.CostRecorded
	usage.CostUSD, usage.CostRecorded = 0, false
	return r.completion.CompleteTurn(ctx, session.CompleteTurnRequest{
		SessionID: bound.ID, TurnID: leased.ID, Message: assistant,
		Artifacts: artifacts, Usage: usage,
		CumulativeCostUSD: cumulativeCost, CostRecorded: costRecorded,
	})
}

func (r *sessionTurnRunner) stageTurnCandidate(
	bound session.Session,
	leased session.Turn,
	assistant string,
	artifacts []session.OutputArtifact,
	usage session.Usage,
	attempt int,
) (session.Turn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sessionACPCompletionTimeout)
	defer cancel()
	digest := sha256.Sum256([]byte(assistant))
	cumulativeCost, costRecorded := usage.CostUSD, usage.CostRecorded
	usage.CostUSD, usage.CostRecorded = 0, false
	return r.store.StageTurnCandidate(ctx, session.StageTurnCandidateRequest{
		SessionID: bound.ID, TurnID: leased.ID, Message: assistant,
		SHA256: fmt.Sprintf("%x", digest[:]), Attempt: attempt,
		Artifacts: artifacts, Usage: usage,
		CumulativeCostUSD: cumulativeCost, CostRecorded: costRecorded,
	})
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
	ctx, cancel := context.WithTimeout(context.Background(), sessionRuntimeReapTimeout)
	defer cancel()
	_, err := r.rt.RemoveByLabel(ctx, box.LabelRun, runID)
	if err != nil {
		return acpFailure(
			sessionACPCleanupError,
			sessionACPBoundedDetail("runtime cleanup failed", err.Error()),
		)
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
		r.cleanupSessionCredentials(bound),
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
	if _, _, _, err := r.runACP(ctx, child, bound, session.Turn{}, "", false); err != nil {
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
	sourceProfile := filepath.Join(sourceRoot, target.Provider, "profiles", account)
	privateProfile := filepath.Join(privateRoot, target.Provider, "profiles", account)
	mcpSnapshot, mcpActive, err := r.captureSessionMCP(sourceRoot, sourceProfile, privateRoot, bound)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDirectory(privateRoot); err != nil {
		return nil, acpFailure(sessionACPCredentialError, "private credential root is unsafe")
	}
	projection := &sessionACPProjection{privateRoot: privateRoot}
	if err := r.projectSessionConfigFiles(sourceRoot, bound, projection, mcpSnapshot, mcpActive); err != nil {
		return projection, err
	}
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
	if live.Prepare != nil {
		if err := live.Prepare(sourceProfile, deadline); err != nil {
			return projection, acpFailure(sessionACPCredentialError, "provider credential needs sign-in or renewal")
		}
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

func (r *sessionTurnRunner) captureSessionMCP(
	sourceRoot string,
	sourceProfile string,
	privateRoot string,
	bound session.Session,
) ([]byte, bool, error) {
	if !bound.ProjectMCP {
		return nil, false, nil
	}
	mcpFile := r.sourceCfg.MCPFile
	if mcpFile == "" {
		mcpFile = filepath.Join(sourceRoot, "mcp.json")
	}
	roots := []box.MCPSourceRoot{
		{Kind: "selected credential home", Path: sourceProfile},
		{Kind: "private session state", Path: privateRoot},
		{Kind: "session workspace", Path: bound.Workspace},
	}
	for _, companion := range bound.Companions {
		roots = append(roots, box.MCPSourceRoot{Kind: "session companion workspace", Path: companion.Workspace})
	}
	resolved, err := box.ResolveMCPSource(mcpFile, roots)
	if err != nil {
		return nil, false, errors.Join(acpFailure(sessionACPCredentialError, "shared MCP source is unsafe"), err)
	}
	readSnapshot := r.readMCPSnapshot
	if readSnapshot == nil {
		readSnapshot = mcp.ReadValidatedSnapshot
	}
	snapshot, active, err := readSnapshot(resolved)
	if err != nil {
		return nil, false, errors.Join(acpFailure(sessionACPCredentialError, "shared MCP config is invalid"), err)
	}
	return snapshot, active, nil
}

func (r *sessionTurnRunner) projectSessionConfigFiles(
	sourceRoot string,
	bound session.Session,
	projection *sessionACPProjection,
	mcpSnapshot []byte,
	mcpActive bool,
) error {
	sources := []struct {
		name string
		path string
	}{
		{name: "env", path: filepath.Join(sourceRoot, "env")},
		{name: "INSTRUCTIONS.md", path: filepath.Join(sourceRoot, "INSTRUCTIONS.md")},
	}
	for _, source := range sources {
		destination := filepath.Join(projection.privateRoot, source.name)
		if err := removeProjectedSessionFile(destination); err != nil {
			return acpFailure(sessionACPCredentialError, "stale private config is unsafe")
		}
		if source.name == "env" && !bound.ProjectEnv {
			continue
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
	mcpDestination := filepath.Join(projection.privateRoot, "mcp.json")
	if err := removeProjectedSessionFile(mcpDestination); err != nil {
		return acpFailure(sessionACPCredentialError, "stale private config is unsafe")
	}
	if mcpActive {
		projection.files = append(projection.files, mcpDestination)
		if err := writeCredentialArtifact(mcpDestination, mcpSnapshot); err != nil {
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
	r.forgetRotation(bound.ID)
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
	ctx, cancel := context.WithTimeout(parent, sessionRuntimeReapTimeout)
	defer cancel()
	if err := box.StopSessionServices(ctx, r.rt, bound.Workspace, bound.Repository); err != nil {
		// Carry the cause: "session services cleanup failed" alone cannot tell a
		// slow daemon from a broken compose project, and the difference is the fix.
		return acpFailure(
			sessionACPCleanupError,
			sessionACPBoundedDetail("session services cleanup failed", err.Error()),
		)
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
	if err := EnsureAncestors(path); err != nil {
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
		bound.Workspace != forkspace.Workspace(bound.Repository, bound.ForkName) ||
		!forkspace.ValidExistingName(bound.ForkName) || bound.Target == "" {
		return nil, acpFailure(sessionACPProcessError, "bound fork identity is invalid")
	}
	if !validSessionRunID(runID) {
		return nil, acpFailure(sessionACPProcessError, "session run identity is invalid")
	}
	env := sessionACPChildEnvironment(
		bound.Repository, bound.Companions, bound.RepositoryReadOnly, privateRoot, runID,
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
	mcpServers, err := r.sessionACPMCPServers(bound, privateRoot)
	if err != nil {
		return nil, err
	}
	process, err := startSessionACPProcess(cmd)
	if process != nil {
		process.runID = runID
		process.mcpServers = mcpServers
	}
	return process, err
}

// sessionACPMCPServers asks the agent for the MCP servers its ACP session has to be handed
// directly, because its adapter cannot read the config file the box mounts (claude — see
// claudeAgent.ACPMCPServers for what that cost in production). Nil for every other agent.
//
// Both inputs come from the session's OWN private root rather than the shared config: a policy
// that withheld env or mcp.json from this session withholds them here too, and that root is
// exactly what the box is started from, so a token resolved here is one the box also has.
func (r *sessionTurnRunner) sessionACPMCPServers(bound session.Session, privateRoot string) ([]map[string]any, error) {
	target, err := agents.ParseTarget(bound.Target)
	if err != nil {
		return nil, acpFailure(sessionACPInvalidTarget, "session target must be one explicit provider and account")
	}
	agent, ok := agents.Get(target.Provider)
	if !ok {
		return nil, acpFailure(sessionACPInvalidTarget, "session target provider is unavailable")
	}
	values := box.EnvFileValues(filepath.Join(privateRoot, "env"))
	servers, err := agent.ACPMCPServers(
		filepath.Join(privateRoot, "mcp.json"),
		func(key string) (string, bool) {
			value, ok := values[key]
			return value, ok
		},
	)
	if err != nil {
		return nil, errors.Join(acpFailure(sessionACPCredentialError, "private MCP projection is invalid"), err)
	}
	return servers, nil
}

// sessionACPMCPServerParam is the "mcpServers" value for session/new and session/load. The
// adapter fingerprints cwd plus this list to decide whether a load can reuse its process, so the
// two requests must carry the identical value — and an empty list, never a null.
func sessionACPMCPServerParam(servers []map[string]any) any {
	if len(servers) == 0 {
		return []any{}
	}
	return servers
}

func sessionACPChildEnvironment(
	repo string,
	companions []session.CompanionRepository,
	repositoryReadOnly bool,
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
	if repositoryReadOnly {
		env = append(env, "COOP_SESSION_REPOSITORY_READ_ONLY=1")
	}
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

func RunIDFromEnv() string {
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
	mcpServers             []map[string]any
	nextID                 int64
	initialized            bool
	nativeSessionID        string
	imageCapable           bool
	embeddedContextCapable bool
	stderr                 *sessionACPStderr
	exited                 atomic.Bool
}

type sessionACPStderr struct {
	mu   sync.Mutex
	tail []byte
}

func (w *sessionACPStderr) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) >= sessionACPStderrLimit {
		w.tail = append(w.tail[:0], p[len(p)-sessionACPStderrLimit:]...)
		return len(p), nil
	}
	overflow := len(w.tail) + len(p) - sessionACPStderrLimit
	if overflow > 0 {
		copy(w.tail, w.tail[overflow:])
		w.tail = w.tail[:len(w.tail)-overflow]
	}
	w.tail = append(w.tail, p...)
	return len(p), nil
}

func (w *sessionACPStderr) String() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.tail)
}

// sessionACPRejectionDetail renders an adapter's JSON-RPC error as a bounded single line.
//
// Every rejection used to collapse to a fixed "ACP request was rejected", which named neither
// the cause nor the fix: a spent quota, a retired model id, and a revoked login were the same
// four words, and the payload was discarded before the turn failed, so no log downstream could
// recover it. The adapter's own `message` is the diagnostic, so it is carried instead of thrown
// away — unlike box stderr, which stays allowlisted in safeSessionACPExitDetail because a
// crashing process can print anything it had in memory.
//
// It is normalised to one line, stripped of control characters, and truncated: it reaches an
// operator through a turn's error detail, and an adapter is not a trusted formatter.
func sessionACPRejectionDetail(raw json.RawMessage) string {
	const rejected = "ACP request was rejected"
	var payload struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return rejected
	}
	return sessionACPBoundedDetail(rejected, payload.Message)
}

// sessionACPBoundedDetail appends a cause to a fixed prefix as one bounded, control-free line —
// the shape a turn's error detail and an operator's log line both need. An empty or
// whitespace-only cause yields just the prefix, never a dangling colon.
func sessionACPBoundedDetail(prefix, cause string) string {
	message := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, cause)
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return prefix
	}
	if len(message) > sessionACPRejectionLimit {
		message = strings.TrimSpace(message[:sessionACPRejectionLimit]) + "…"
	}
	return prefix + ": " + message
}

func safeSessionACPExitDetail(stderr string) string {
	text := strings.ToLower(stderr)
	switch {
	case strings.Contains(text, "image \"") && strings.Contains(text, "not built") && strings.Contains(text, "coop build"):
		return "Coop box image is not built; run 'coop build'"
	case strings.Contains(text, "no space left on device"):
		return "Coop runtime storage is full"
	case strings.Contains(text, "cannot connect to the docker daemon") || strings.Contains(text, "is the docker daemon running"):
		return "Coop cannot reach the Docker runtime"
	case strings.Contains(text, "not authenticated") && strings.Contains(text, "coop login"):
		return "the configured Coop account is not authenticated; run 'coop login'"
	default:
		return ""
	}
}

func sessionACPChildClosedFailure(process *sessionACPProcess) error {
	detail := "ACP child closed before its response"
	if process != nil {
		if diagnostic := safeSessionACPExitDetail(process.stderr.String()); diagnostic != "" {
			detail += ": " + diagnostic
		}
	}
	return acpFailure(sessionACPProcessError, detail)
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
		frames: make(chan sessionACPFrame, 1), stderr: &sessionACPStderr{},
	}
	cmd.Stderr = process.stderr
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

func (r *sessionTurnRunner) runACP(
	ctx context.Context,
	process *sessionACPProcess,
	bound session.Session,
	leased session.Turn,
	promptText string,
	checkpointSend bool,
) (string, []session.OutputArtifact, session.Usage, error) {
	if process == nil || process.frames == nil {
		return "", nil, session.Usage{}, acpFailure(sessionACPProcessError, "ACP child is unavailable")
	}
	frames := process.frames
	// The adapter's own rate-limit markers, resolved from the rung this child is running.
	limitTarget, _ := agents.ParseTarget(bound.Target)
	limitProvider := limitTarget.Provider
	var transcriptBytes int
	var assistant []byte
	var outputArtifacts []session.OutputArtifact
	var cumulativeCostUSD float64
	var costRecorded bool
	collectAssistant := false
	// Narration is bound to the admitted prompt for the same reason assistant
	// text is: startup, auth, and status frames are not work the caller asked
	// for. close() runs before this function returns, so every activity event
	// is sequenced below the turn.completed that completeTurn appends after it.
	activity := newSessionActivity(r.store, bound.ID, leased.ID, r.activityClock)
	defer func() {
		drainCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), sessionACPCleanupTimeout,
		)
		activity.close(drainCtx)
		cancel()
	}()
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
					if amount, ok := sessionACPReportedCost(envelope.Params); ok {
						cumulativeCostUSD, costRecorded = amount, true
					}
					activity.observe(envelope.Params)
				}
				transcriptBytes += sessionACPUpdateTranscriptBytes(
					envelope.Params,
					len(frame)+1,
					imageFrame,
				)
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
					Options  []permOption `json:"options"`
					ToolCall struct {
						ToolCallID string `json:"toolCallId"`
					} `json:"toolCall"`
				}
				if err := json.Unmarshal(envelope.Params, &params); err != nil {
					return nil, "", acpFailure(sessionACPProtocolError, "malformed permission request")
				}
				outcome := map[string]any{"outcome": "cancelled"}
				decision := "cancelled"
				option := chooseSessionACPAllow(params.Options)
				if option != "" {
					outcome = map[string]any{"outcome": "selected", "optionId": option}
					decision = "selected"
				}
				if collectAssistant {
					// Nobody human answered this. Which way the policy answered
					// on their behalf is exactly the kind of fact a trace owes.
					activity.permission(
						params.ToolCall.ToolCallID, decision,
						option, permOptionKind(params.Options, option),
					)
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
						return nil, sessionACPChildClosedFailure(process)
					}
					return nil, frame.err
				}
				responseID, result, err := handle(frame.line, expectedSession)
				if err != nil {
					return nil, err
				}
				// After handling, and only for the admitted prompt: startup and auth frames
				// are not the turn's progress, and a frame that narrated something has
				// already moved the window it would otherwise be announced in.
				if collectAssistant {
					activity.frame(len(frame.line))
				}
				if responseID != nil && bytes.Equal(bytes.TrimSpace(responseID), bytes.TrimSpace(id)) {
					var envelope struct {
						Error json.RawMessage `json:"error"`
					}
					if err := json.Unmarshal(frame.line, &envelope); err != nil {
						return nil, acpFailure(sessionACPProtocolError, "malformed ACP response")
					}
					if len(envelope.Error) != 0 && string(envelope.Error) != "null" {
						// A rate limit is the one rejection a target ladder can act on, so it is
						// classified here instead of being flattened into the generic rejection.
						// Output exhaustion is excluded: the same rung can continue that turn.
						if hint := ladder.ACPErrorLimitHint(
							envelope.Error, time.Now(), ladder.ACPRateSignals(limitProvider),
						); hint.Limited && !hint.OutputLimited {
							return nil, &sessionACPFailure{
								code:    sessionACPRateLimited,
								detail:  "provider rate limited the turn",
								resetAt: hint.ResetAt,
							}
						}
						return nil, acpFailure(
							sessionACPProtocolError, sessionACPRejectionDetail(envelope.Error),
						)
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
			return "", nil, session.Usage{}, err
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
			return "", nil, session.Usage{}, acpFailure(sessionACPProtocolError, "ACP protocol version is unsupported")
		}
		process.imageCapable = initialized.AgentCapabilities.PromptCapabilities.Image
		process.embeddedContextCapable = initialized.AgentCapabilities.PromptCapabilities.EmbeddedContext
		mcpServers := sessionACPMCPServerParam(process.mcpServers)
		nativeID := bound.NativeSessionID
		if nativeID == "" {
			result, err := request("session/new", map[string]any{"cwd": bound.Workspace, "mcpServers": mcpServers}, "")
			if err != nil {
				return "", nil, session.Usage{}, err
			}
			var created struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(result, &created) != nil || !validACPSessionID(created.SessionID) {
				return "", nil, session.Usage{}, acpFailure(sessionACPProtocolError, "session/new returned an invalid session id")
			}
			nativeID = created.SessionID
		} else if !validACPSessionID(nativeID) {
			return "", nil, session.Usage{}, acpFailure(sessionACPProtocolError, "stored native session id is invalid")
		} else if _, err := request("session/load", map[string]any{"sessionId": nativeID, "cwd": bound.Workspace, "mcpServers": mcpServers}, nativeID); err != nil {
			return "", nil, session.Usage{}, err
		}
		process.nativeSessionID = nativeID
		process.initialized = true
	}
	nativeID := process.nativeSessionID
	if !validACPSessionID(nativeID) || (bound.NativeSessionID != "" && bound.NativeSessionID != nativeID) {
		return "", nil, session.Usage{}, acpFailure(sessionACPProtocolError, "warm ACP session identity changed")
	}
	if leased.ID == "" {
		return "", nil, session.Usage{}, nil
	}
	if bound.NativeSessionID == "" {
		ctxBind, cancel := context.WithTimeout(context.Background(), sessionACPCleanupTimeout)
		_, err := r.store.BindNativeSession(ctxBind, bound.ID, nativeID)
		cancel()
		if err != nil {
			return "", nil, session.Usage{}, err
		}
	}
	if checkpointSend {
		checkpointCtx, checkpointCancel := context.WithTimeout(context.Background(), sessionACPCleanupTimeout)
		_, err := r.store.MarkTurnSendIntent(checkpointCtx, bound.ID, leased.ID)
		checkpointCancel()
		if err != nil {
			return "", nil, session.Usage{}, acpFailure(session.CodeInternal, "turn sent checkpoint failed")
		}
	}
	outputDir, outputRelative, err := prepareSessionOutputDir(bound.Workspace, leased.ID)
	if err != nil {
		return "", nil, session.Usage{}, acpFailure(sessionACPProtocolError, "turn output directory could not be prepared")
	}
	defer removeSessionOutputDir(outputDir)
	content, err := sessionACPInputContent(leased, process.imageCapable, process.embeddedContextCapable)
	if err != nil {
		return "", nil, session.Usage{}, err
	}
	content[0]["text"] = fmt.Sprintf("<coop-output>Save only final generated images and charts in %s. Use PNG, JPEG, WebP, or GIF; at most %d files and %d bytes total. Keep source data, virtual environments, caches, and other scratch content outside this directory. Do not put image bytes or data URLs in your reply. Refer to saved filenames in the structured response when the caller requests visuals. Direct image outputs returned by tools are captured in order as generated-1.png (or the matching image extension), generated-2.png, and so on.</coop-output>\n\n%s", outputRelative, session.MaxTurnArtifacts, session.MaxTurnArtifactBytes, promptText)
	prompt := map[string]any{"sessionId": nativeID, "prompt": content}
	id := next()
	if err := writeRequest(id, "session/prompt", prompt); err != nil {
		return "", nil, session.Usage{}, err
	}
	if checkpointSend {
		checkpointCtx, checkpointCancel := context.WithTimeout(context.Background(), sessionACPCleanupTimeout)
		_, err = r.store.MarkTurnSent(checkpointCtx, bound.ID, leased.ID)
		checkpointCancel()
		if err != nil {
			return "", nil, session.Usage{}, err
		}
	}
	collectAssistant = true
	result, err := waitResponse(id, nativeID, true)
	if err != nil {
		return "", nil, session.Usage{}, err
	}
	// Usage, when the adapter reports it. ACP does not require it, and which
	// adapters populate it is not something this code can assume — so it is
	// read from both the documented _meta bag and a top-level field, and left
	// at zero otherwise. A caller distinguishes "nothing reported" from "free"
	// through Usage.Recorded rather than by reading a zero as a measurement.
	var promptResult struct {
		StopReason string    `json:"stopReason"`
		Usage      *acpUsage `json:"usage"`
		Meta       *struct {
			Usage *acpUsage `json:"usage"`
		} `json:"_meta"`
	}
	if json.Unmarshal(result, &promptResult) != nil || !validACPStopReason(promptResult.StopReason) {
		return "", nil, session.Usage{}, acpFailure(sessionACPProtocolError, "session/prompt returned an invalid stop reason")
	}
	if promptResult.StopReason == "cancelled" || promptResult.StopReason == "error" {
		return "", nil, session.Usage{}, acpFailure(sessionACPCancelledError, "ACP prompt was cancelled")
	}
	if len(assistant) > sessionACPMessageLimit || !utf8.Valid(assistant) {
		return "", nil, session.Usage{}, acpFailure(sessionACPProtocolError, "assistant message exceeded its bound")
	}
	payload, err := json.Marshal(map[string]any{"text": string(assistant), "final": true})
	if err != nil || len(payload) > session.MaxEventPayloadBytes {
		return "", nil, session.Usage{}, acpFailure(sessionACPProtocolError, "assistant message exceeded its durable event bound")
	}
	files, err := collectSessionOutputDir(outputDir)
	if err != nil {
		return "", nil, session.Usage{}, acpFailure(sessionACPProtocolError, err.Error())
	}
	for _, artifact := range files {
		outputArtifacts = appendOutputArtifact(outputArtifacts, artifact.Name, artifact.MediaType, artifact.Data)
	}
	if len(outputArtifacts) > session.MaxTurnArtifacts {
		return "", nil, session.Usage{}, acpFailure(sessionACPProtocolError, "turn produced too many output artifacts")
	}
	total := 0
	for _, artifact := range outputArtifacts {
		total += len(artifact.Data)
	}
	if total > session.MaxTurnArtifactBytes {
		return "", nil, session.Usage{}, acpFailure(sessionACPProtocolError, "turn output artifacts exceed their total bound")
	}
	// The whole point of parsing it. The first version read the usage into a
	// struct and returned only the message, so CompleteTurnRequest.Usage was
	// never set and every turn persisted four zeros — plumbing that existed,
	// compiled, and carried nothing.
	usage := promptResult.Usage.session()
	if !usage.Recorded() && promptResult.Meta != nil {
		usage = promptResult.Meta.Usage.session()
	}
	usage.CostUSD, usage.CostRecorded = cumulativeCostUSD, costRecorded
	return string(assistant), outputArtifacts, usage, nil
}

func sessionACPUpdateTranscriptBytes(raw json.RawMessage, frameBytes int, imageFrame bool) int {
	if imageFrame {
		return 512
	}
	var envelope struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
		} `json:"update"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return frameBytes
	}
	switch envelope.Update.SessionUpdate {
	case "assistant_message_chunk", "agent_message_chunk":
		return frameBytes
	default:
		// Tool updates are inspected one frame at a time and then discarded. Charging their
		// cumulative bytes as retained transcript makes legitimate long turns fail without
		// adding a memory-safety boundary; the per-frame and turn deadline bounds still apply.
		return 0
	}
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

// permOption is one choice in a session/request_permission request (ACP kinds: allow_once,
// allow_always, reject_once, reject_always). A four-line view of an EXTERNAL wire message, not
// shared logic: the ACP control plane decodes the same shape into its own struct and chooses with
// its own rules, which do not carry the id validation below.
type permOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
}

// permOptionKind names the kind of the option that was actually chosen, so a
// recorded permission says "allow_always" rather than only an opaque id.
func permOptionKind(options []permOption, optionID string) string {
	if optionID == "" {
		return ""
	}
	for _, option := range options {
		if option.OptionID == optionID {
			return option.Kind
		}
	}
	return ""
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
