package session

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"
)

// Each round reports a native-session counter; public turn receipts report only
// that logical turn's spend, even through semantic repair and provider rotation.
func TestCandidateCostSurvivesEveryTerminalPath(t *testing.T) {
	for _, finish := range []string{"accept", "reject", "fail", "cancel", "interrupt", "exhaust"} {
		t.Run(finish, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
			defer store.Close()
			sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{JobDocument: testJobDocument, JobDigest: testJobDigest, Target: "claude:model", MaxTurns: 10})
			if err != nil {
				t.Fatal(err)
			}
			lease := func(key string, semantic bool) Turn {
				t.Helper()
				current, err := store.GetSession(ctx, sess.ID)
				if err != nil {
					t.Fatal(err)
				}
				req := SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: current.Revision, Prompt: key}
				if semantic {
					schema := []byte(`{"type":"object"}`)
					req.OutputContract = &OutputContract{JSONSchema: schema, SHA256: fmt.Sprintf("%x", sha256.Sum256(schema)), RequireSemanticValidation: true}
				}
				if _, err := store.SubmitTurn(ctx, key, req); err != nil {
					t.Fatal(err)
				}
				turn, ok, err := store.LeaseNextTurn(ctx, sess.ID)
				if err != nil || !ok {
					t.Fatalf("lease: %v %v", ok, err)
				}
				if _, err = store.MarkTurnSendIntent(ctx, sess.ID, turn.ID); err != nil {
					t.Fatal(err)
				}
				turn, err = store.MarkTurnSent(ctx, sess.ID, turn.ID)
				if err != nil {
					t.Fatal(err)
				}
				return turn
			}
			first := lease("first", false)
			if _, err = store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: first.ID, Message: "first", CumulativeCostUSD: 1, CostRecorded: true}); err != nil {
				t.Fatal(err)
			}
			turn := lease("repair", true)
			candidate := `{"answer":"candidate"}`
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(candidate)))
			attempt := 1
			if finish == "reject" {
				attempt = 3
			}
			turn, err = store.StageTurnCandidate(ctx, StageTurnCandidateRequest{SessionID: sess.ID, TurnID: turn.ID, Message: candidate, SHA256: digest, Attempt: attempt, Usage: Usage{InputTokens: 10}, CumulativeCostUSD: 1.5, CostRecorded: true})
			if err != nil {
				t.Fatal(err)
			}
			if !turn.Usage.CostRecorded || turn.Usage.CostUSD != 0.5 {
				t.Errorf("staged turn cost = %+v, want0.5", turn.Usage)
			}
			switch finish {
			case "accept":
				turn, err = store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: turn.ID, CandidateSHA256: digest})
			case "reject":
				turn, err = store.RejectTurnCandidate(ctx, RejectTurnCandidateRequest{SessionID: sess.ID, TurnID: turn.ID, CandidateSHA256: digest, Violations: []string{"incorrect"}})
			case "cancel":
				current, _ := store.GetSession(ctx, sess.ID)
				turn, err = store.CancelTurn(ctx, "cancel", CancelTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ExpectedRevision: current.Revision})
			default:
				turn, err = store.RejectTurnCandidate(ctx, RejectTurnCandidateRequest{SessionID: sess.ID, TurnID: turn.ID, CandidateSHA256: digest, Violations: []string{"incorrect"}})
				if err != nil {
					t.Fatal(err)
				}
				if finish == "exhaust" {
					current, _ := store.GetSession(ctx, sess.ID)
					_, err = store.ExhaustBudget(ctx, "exhaust", ExhaustBudgetRequest{SessionID: sess.ID, ExpectedRevision: current.Revision})
				} else {
					var ok bool
					turn, ok, err = store.LeaseNextTurn(ctx, sess.ID)
					if err != nil || !ok {
						t.Fatalf("repair lease: %v %v", ok, err)
					}
					if finish == "fail" {
						turn, err = store.FailTurn(ctx, FailTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ErrorCode: CodeInternal, ErrorDetail: "failed"})
					} else {
						_, err = store.ReconcileInterruptedTurns(ctx)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.GetTurn(ctx, sess.ID, turn.ID)
			if err != nil || !got.Usage.CostRecorded || got.Usage.CostUSD != 0.5 || got.Usage.InputTokens != 10 {
				t.Fatalf("settled %s usage = %+v, %v", finish, got.Usage, err)
			}
			if finish != "exhaust" {
				next := lease("next", false)
				next, err = store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: next.ID, Message: "next", CumulativeCostUSD: 1.75, CostRecorded: true})
				if err != nil || next.Usage.CostUSD != 0.25 {
					t.Fatalf("following turn = %+v, %v", next.Usage, err)
				}
			}
		})
	}
}

func TestCostCheckpointsAcrossProviderEpochs(t *testing.T) {
	for _, mode := range []string{"same provider", "new provider", "new provider unknown", "reset", "zero", "crash after rotation"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "state")
			store := openTestStore(t, root)
			defer func() { _ = store.Close() }()
			sess, err := store.CreateSession(ctx, "create", CreateSessionRequest{JobDocument: testJobDocument, JobDigest: testJobDigest, Target: "claude:a", MaxTurns: 10})
			if err != nil {
				t.Fatal(err)
			}
			lease := func(key string) Turn {
				t.Helper()
				current, err := store.GetSession(ctx, sess.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = store.SubmitTurn(ctx, key, SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: current.Revision, Prompt: key}); err != nil {
					t.Fatal(err)
				}
				turn, ok, err := store.LeaseNextTurn(ctx, sess.ID)
				if err != nil || !ok {
					t.Fatalf("lease: %v %v", ok, err)
				}
				if _, err = store.MarkTurnSendIntent(ctx, sess.ID, turn.ID); err != nil {
					t.Fatal(err)
				}
				turn, err = store.MarkTurnSent(ctx, sess.ID, turn.ID)
				if err != nil {
					t.Fatal(err)
				}
				return turn
			}
			first := lease("first")
			if _, err = store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: first.ID, Message: "first", CumulativeCostUSD: 1, CostRecorded: true}); err != nil {
				t.Fatal(err)
			}
			turn := lease("work")
			report := func(raw float64, known bool, tokens int) Usage {
				t.Helper()
				usage, err := store.RecordTurnUsage(ctx, RecordTurnUsageRequest{SessionID: sess.ID, TurnID: turn.ID, Usage: Usage{InputTokens: tokens}, CumulativeCostUSD: raw, CostRecorded: known})
				if err != nil {
					t.Fatal(err)
				}
				return usage
			}
			for range 2 {
				if got := report(1.5, true, 10); got.CostUSD != 0.5 || got.InputTokens != 10 {
					t.Fatalf("repeated checkpoint: %+v", got)
				}
			}
			want, nextRaw := 0.75, 2.0
			switch mode {
			case "same provider", "new provider", "new provider unknown", "crash after rotation":
				reset := mode != "same provider"
				if _, _, err = store.RotateTurnTarget(ctx, sess.ID, turn.ID, "claude:a", "codex:b", reset); err != nil {
					t.Fatal(err)
				}
				if mode == "crash after rotation" {
					if err = store.Close(); err != nil {
						t.Fatal(err)
					}
					store = openTestStore(t, root)
					recovered, err := store.ReconcileInterruptedTurns(ctx)
					if err != nil || len(recovered) != 1 || recovered[0].State != TurnQueued || recovered[0].Usage.CostUSD != 0.5 {
						t.Fatalf("recovery: %+v %v", recovered, err)
					}
					var ok bool
					turn, ok, err = store.LeaseNextTurn(ctx, sess.ID)
					if err != nil || !ok {
						t.Fatal(err)
					}
				}
				if _, err = store.MarkTurnSendIntent(ctx, sess.ID, turn.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = store.MarkTurnSent(ctx, sess.ID, turn.ID); err != nil {
					t.Fatal(err)
				}
				if mode == "same provider" {
					report(1.75, true, 20)
				} else if mode == "new provider unknown" {
					report(0, false, 20)
					want, nextRaw = 0.5, 0.25
				} else {
					report(2, true, 20)
					want, nextRaw = 2.5, 2.25
				}
				if _, _, err = store.RotateTurnTarget(ctx, sess.ID, turn.ID, "claude:a", "codex:b", true); CodeOf(err) != CodeRevisionConflict {
					t.Fatalf("stale rotation changed cursor: %v", err)
				}
			case "reset":
				report(0.25, true, 20)
				report(0.5, true, 30)
				want, nextRaw = 1, 0.75
			case "zero":
				report(0, true, 20)
				want, nextRaw = 0.5, 0.25
			}
			done, err := store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: turn.ID, Message: "done"})
			if err != nil || !done.Usage.CostRecorded || done.Usage.CostUSD != want {
				t.Fatalf("%s cost: %+v %v", mode, done.Usage, err)
			}
			if _, err = store.RecordTurnUsage(ctx, RecordTurnUsageRequest{SessionID: sess.ID, TurnID: turn.ID, CumulativeCostUSD: 99, CostRecorded: true}); CodeOf(err) != CodeTurnNotRunnable {
				t.Fatalf("terminal checkpoint accepted: %v", err)
			}
			next := lease("next")
			next, err = store.CompleteTurn(ctx, CompleteTurnRequest{SessionID: sess.ID, TurnID: next.ID, Message: "next", CumulativeCostUSD: nextRaw, CostRecorded: true})
			if err != nil || next.Usage.CostUSD != 0.25 {
				t.Fatalf("next cost: %+v %v", next.Usage, err)
			}
		})
	}
}
