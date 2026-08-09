package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// The operations table is a write-ahead intent record: MarkOperationRunning durably records
// what a cross-process operation is about to do, and MarkOperationUncertain records that its
// outcome became unknown, specifically so a daemon that crashes mid-operation can recover
// instead of silently losing or double-applying the side effect. These tests simulate that
// crash by closing and reopening the store — a real process boundary, not just an in-memory
// handle — then verify the durable state each intermediate stage leaves behind, and how a
// recovering daemon is expected to resolve it.
func TestOperationCrashReplayAtEachIntermediateStateReturnsUncertain(t *testing.T) {
	ctx := context.Background()
	for _, state := range []OperationState{OperationReserved, OperationRunning, OperationUncertain} {
		t.Run(string(state), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			store := openTestStore(t, root)

			// Reserve exactly the way CreateSession would, so a later retry through the
			// public API lands on this same operation.
			req := normalizeCreateRequest(CreateSessionRequest{Target: "codex"})
			key := "crash-" + string(state)
			op, replay, err := store.ReserveOperation(ctx, "CreateSession", key, req)
			if err != nil || replay {
				t.Fatalf("reserve %s = %+v, replay=%v, err=%v", state, op, replay, err)
			}
			intent := mustJSON(map[string]string{"attempt": string(state)})
			switch state {
			case OperationReserved:
				// The crash lands before the operation ever starts.
			case OperationRunning:
				if err := store.MarkOperationRunning(ctx, op.ID, intent); err != nil {
					t.Fatal(err)
				}
			case OperationUncertain:
				if err := store.MarkOperationRunning(ctx, op.ID, intent); err != nil {
					t.Fatal(err)
				}
				if err := store.MarkOperationUncertain(ctx, op.ID); err != nil {
					t.Fatal(err)
				}
			}

			// Simulate the crash: close the handle and reopen a fresh Store against the same
			// on-disk state, exactly as a restarted daemon would.
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			recovered := openTestStore(t, root)
			defer recovered.Close()

			got, err := recovered.GetOperationByID(ctx, op.ID)
			if err != nil {
				t.Fatalf("recovered operation: %v", err)
			}
			if got.State != state {
				t.Fatalf("recovered state = %s, want %s", got.State, state)
			}
			if state != OperationReserved && string(got.Result) != string(intent) {
				t.Fatalf("recovered intent = %q, want %q", got.Result, intent)
			}

			// The documented resolution for a retry landing on any non-terminal state: never
			// a silent false success or failure, always "the outcome is unknown, come back."
			if _, err := recovered.CreateSession(ctx, key, CreateSessionRequest{Target: "codex"}); !errors.Is(err, ErrOperationUncertain) {
				t.Fatalf("replay at %s = %v, want ErrOperationUncertain", state, err)
			}

			// Recovery resolves it explicitly, exactly like a restarted daemon reconciling
			// against the real world (e.g. finding the fork it was about to create) would.
			result := []byte(`{"id":"recovered-session"}`)
			if err := recovered.CompleteOperation(ctx, op.ID, "session", "recovered-session", result); err != nil {
				t.Fatalf("resolve %s: %v", state, err)
			}
			resolved, err := recovered.GetOperationByID(ctx, op.ID)
			if err != nil || resolved.State != OperationSucceeded || resolved.ResourceID != "recovered-session" {
				t.Fatalf("resolved %s operation = %+v, err=%v", state, resolved, err)
			}

			// A further replay now returns the real, resolved result instead of uncertainty.
			final, err := recovered.CreateSession(ctx, key, CreateSessionRequest{Target: "codex"})
			if err != nil || final.ID != "recovered-session" {
				t.Fatalf("final replay after resolution = %+v, err=%v", final, err)
			}
		})
	}
}

// driveOperationTo reserves a fresh operation and advances it to the given non-terminal state
// via the same primitives a real caller would use, returning the operation as durably observed
// after that transition (so its recorded intent, if any, is exactly what a reader would see).
func driveOperationTo(t *testing.T, ctx context.Context, store *Store, state OperationState, key string) Operation {
	t.Helper()
	op, replay, err := store.ReserveOperation(ctx, "Journal", key, map[string]string{"key": key})
	if err != nil || replay {
		t.Fatalf("reserve %s = %+v, replay=%v, err=%v", key, op, replay, err)
	}
	if state == OperationReserved {
		return op
	}
	intent := mustJSON(map[string]string{"intent": key})
	if err := store.MarkOperationRunning(ctx, op.ID, intent); err != nil {
		t.Fatalf("mark %s running: %v", key, err)
	}
	if state == OperationRunning {
		got, err := store.GetOperationByID(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if err := store.MarkOperationUncertain(ctx, op.ID); err != nil {
		t.Fatalf("mark %s uncertain: %v", key, err)
	}
	got, err := store.GetOperationByID(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The journal's two exits — CompleteOperation and FailOperation — must be reachable from every
// non-terminal state, must reject a second call once terminal, and FailOperation must never
// clobber the intent a prior MarkOperationRunning recorded (a recovering caller still needs to
// see what was attempted, not just that it failed).
func TestCompleteAndFailOperationFromEveryNonTerminalState(t *testing.T) {
	ctx := context.Background()
	for _, state := range []OperationState{OperationReserved, OperationRunning, OperationUncertain} {
		t.Run(string(state)+"/complete", func(t *testing.T) {
			store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
			defer store.Close()
			op := driveOperationTo(t, ctx, store, state, "complete-"+string(state))
			if err := store.CompleteOperation(ctx, op.ID, "session", "res-1", []byte(`{"ok":true}`)); err != nil {
				t.Fatalf("complete from %s: %v", state, err)
			}
			got, err := store.GetOperationByID(ctx, op.ID)
			if err != nil || got.State != OperationSucceeded || got.ResourceType != "session" ||
				got.ResourceID != "res-1" || string(got.Result) != `{"ok":true}` {
				t.Fatalf("completed from %s = %+v, err=%v", state, got, err)
			}
			if err := store.CompleteOperation(ctx, op.ID, "session", "res-2", []byte(`{}`)); CodeOf(err) != CodeInvalidRequest {
				t.Fatalf("re-complete terminal op = %v", err)
			}
			if err := store.FailOperation(ctx, op.ID, CodeInternal, "too late"); CodeOf(err) != CodeInvalidRequest {
				t.Fatalf("fail terminal op = %v", err)
			}
		})
		t.Run(string(state)+"/fail", func(t *testing.T) {
			store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
			defer store.Close()
			op := driveOperationTo(t, ctx, store, state, "fail-"+string(state))
			if err := store.FailOperation(ctx, op.ID, CodeInternal, "boom"); err != nil {
				t.Fatalf("fail from %s: %v", state, err)
			}
			got, err := store.GetOperationByID(ctx, op.ID)
			if err != nil || got.State != OperationFailed || got.ErrorCode != CodeInternal || got.ErrorDetail != "boom" {
				t.Fatalf("failed from %s = %+v, err=%v", state, got, err)
			}
			if string(got.Result) != string(op.Result) {
				t.Fatalf("failure lost the recorded intent: before=%q after=%q", op.Result, got.Result)
			}
			if err := store.CompleteOperation(ctx, op.ID, "session", "res", []byte(`{}`)); CodeOf(err) != CodeInvalidRequest {
				t.Fatalf("complete terminal op = %v", err)
			}
			if err := store.FailOperation(ctx, op.ID, CodeInternal, "again"); CodeOf(err) != CodeInvalidRequest {
				t.Fatalf("re-fail terminal op = %v", err)
			}
		})
	}
}

// Two transitions the state machine deliberately refuses even though they look adjacent:
// "reserved" cannot go straight to "uncertain" (nothing was attempted, so there is nothing to
// be uncertain about — MarkOperationRunning is the only valid next step), and once "uncertain"
// an operation cannot go back to "running" (its intent must be resolved via Complete/Fail, not
// re-attempted, since the caller no longer knows whether the original attempt took effect).
func TestOperationInvalidTransitionsAreRejected(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()

	reserved, _, err := store.ReserveOperation(ctx, "Journal", "reserved-to-uncertain", map[string]string{"a": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOperationUncertain(ctx, reserved.ID); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("reserved->uncertain = %v, want CodeInvalidRequest", err)
	}

	uncertain, _, err := store.ReserveOperation(ctx, "Journal", "uncertain-to-running", map[string]string{"a": "2"})
	if err != nil {
		t.Fatal(err)
	}
	intent := mustJSON(map[string]string{"x": "y"})
	if err := store.MarkOperationRunning(ctx, uncertain.ID, intent); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOperationUncertain(ctx, uncertain.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOperationRunning(ctx, uncertain.ID, intent); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("uncertain->running = %v, want CodeInvalidRequest", err)
	}
}
