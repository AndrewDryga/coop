package session

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationConvertsPendingCostsWithoutRewritingReceipts(t *testing.T) {
	for _, state := range []TurnState{TurnQueued, TurnStarting, TurnRunning, TurnAwaitingValidation} {
		for _, epoch := range []string{"same", "reset before", "reset after", "zero", "unknown"} {
			t.Run(string(state)+"/"+epoch, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "state")
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, databaseName)
				buildLegacyDatabase(t, path, 25)
				db, err := sql.Open("sqlite", "file:"+path)
				if err != nil {
					t.Fatal(err)
				}
				raw, known, want, cursor, cursorKnown := 1.5, true, 0.5, 1.5, true
				switch epoch {
				case "reset before":
					raw, want, cursor = 2, 2, 2
				case "reset after":
					cursor, cursorKnown = 0, false
				case "zero":
					raw, want, cursor = 0, 0, 0
				case "unknown":
					raw, known, want, cursor = 0, false, 0, 1
				}
				if _, err = db.Exec(`UPDATE sessions SET usage_cumulative_cost_usd=1,usage_cost_recorded=1,active_turn_id='turn-legacy',activity='running' WHERE id='legacy-session'`); err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(`UPDATE turns SET state=?,send_state='sent',usage_cost_usd=?,usage_cost_recorded=?,usage_input_tokens=10 WHERE id='turn-legacy'`, string(state), raw, known); err != nil {
					t.Fatal(err)
				}
				const receipt = `{"usage":{"cost_usd":1.5,"cost_recorded":true},"state":"awaiting_validation"}`
				if _, err = db.Exec(`UPDATE operations SET result=? WHERE id='op-legacy'`, []byte(receipt)); err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(`INSERT INTO turns(id,session_id,ordinal,idempotency_key,request_hash,state,send_state,prompt,queued_at,usage_cost_usd,usage_cost_recorded) VALUES ('historic','legacy-session',2,'historic','hash','completed','sent','historic',1,0.75,1)`); err != nil {
					t.Fatal(err)
				}
				event := func(seq int, kind, payload string) {
					t.Helper()
					if _, err := db.Exec(`INSERT INTO events(id,session_id,sequence,turn_id,type,version,occurred_at,payload) VALUES (?,'legacy-session',?,'turn-legacy',?,1,1,?)`, fmt.Sprintf("event%d", seq), seq, kind, []byte(payload)); err != nil {
						t.Fatal(err)
					}
				}
				if epoch == "reset before" {
					event(1, "session.target_rotated", `{"native_session_reset":true}`)
				}
				event(2, "turn.awaiting_validation", `{"attempt":1}`)
				if epoch == "reset after" {
					event(3, "session.target_rotated", `{"native_session_reset":true}`)
				}
				if err = db.Close(); err != nil {
					t.Fatal(err)
				}
				for reopen := 0; reopen < 2; reopen++ {
					store := openTestStore(t, root)
					turn, err := store.GetTurn(context.Background(), "legacy-session", "turn-legacy")
					if err != nil || turn.Usage.CostUSD != want || turn.Usage.CostRecorded != known || turn.Usage.InputTokens != 10 {
						t.Fatalf("migrated usage: %+v %v", turn.Usage, err)
					}
					var gotCursor float64
					var gotKnown bool
					if err = store.db.QueryRow(`SELECT usage_cumulative_cost_usd,usage_cost_recorded FROM sessions WHERE id='legacy-session'`).Scan(&gotCursor, &gotKnown); err != nil || gotCursor != cursor || gotKnown != cursorKnown {
						t.Fatalf("cursor=%v/%v want%v/%v: %v", gotCursor, gotKnown, cursor, cursorKnown, err)
					}
					op, err := store.GetOperation(context.Background(), "legacy-key")
					if err != nil || string(op.Result) != receipt {
						t.Fatalf("historical receipt changed: %s %v", op.Result, err)
					}
					history, err := store.GetTurn(context.Background(), "legacy-session", "historic")
					if err != nil || history.Usage.CostUSD != 0.75 {
						t.Fatalf("historical cost changed: %+v %v", history.Usage, err)
					}
					if reopen == 1 && state == TurnRunning {
						next := cursor + 0.25
						if !cursorKnown {
							next = 0.25
						}
						got, err := store.RecordTurnUsage(context.Background(), RecordTurnUsageRequest{SessionID: "legacy-session", TurnID: "turn-legacy", CumulativeCostUSD: next, CostRecorded: true})
						if err != nil || got.CostUSD != want+0.25 {
							t.Fatalf("continued migrated usage: %+v %v", got, err)
						}
					}
					if err = store.Close(); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestMigrationRetiresProvablyStaleCostCursorAcrossTurns(t *testing.T) {
	for _, mode := range []string{"idle", "pending later turn", "ambiguous later terminal"} {
		t.Run(mode, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, databaseName)
			buildLegacyDatabase(t, path, 25)
			db, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := db.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`UPDATE sessions SET usage_cumulative_cost_usd=1,usage_cost_recorded=1 WHERE id='legacy-session'`)
			exec(`UPDATE turns SET state='failed',send_state='sent' WHERE id='turn-legacy'`)
			exec(`INSERT INTO events(id,session_id,sequence,turn_id,type,version,occurred_at,payload) VALUES ('reset','legacy-session',1,'turn-legacy','session.target_rotated',1,1,?)`, []byte(`{"native_session_reset":true}`))
			exec(`INSERT INTO events(id,session_id,sequence,turn_id,type,version,occurred_at,payload) VALUES ('failed','legacy-session',2,'turn-legacy','turn.failed',1,2,'{}')`)
			if mode == "pending later turn" {
				exec(`INSERT INTO turns(id,session_id,ordinal,idempotency_key,request_hash,state,send_state,prompt,queued_at,usage_cost_usd,usage_cost_recorded) VALUES ('pending','legacy-session',2,'pending','hash','running','sent','next',3,2,1)`)
				exec(`INSERT INTO events(id,session_id,sequence,turn_id,type,version,occurred_at,payload) VALUES ('candidate','legacy-session',3,'pending','turn.awaiting_validation',1,3,'{}')`)
				exec(`UPDATE sessions SET active_turn_id='pending',activity='running' WHERE id='legacy-session'`)
			} else if mode == "ambiguous later terminal" {
				exec(`UPDATE turns SET usage_cost_usd=0.25,usage_cost_recorded=1 WHERE id='turn-legacy'`)
			}
			exec(`UPDATE sessions SET last_event_sequence=3 WHERE id='legacy-session'`)
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			store := openTestStore(t, root)
			defer store.Close()
			var cursor float64
			var known bool
			if err = store.db.QueryRow(`SELECT usage_cumulative_cost_usd,usage_cost_recorded FROM sessions WHERE id='legacy-session'`).Scan(&cursor, &known); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "idle":
				if cursor != 0 || known {
					t.Fatalf("stale cursor retained: %v %v", cursor, known)
				}
				turn, err := store.SubmitTurn(context.Background(), "next", SubmitTurnRequest{SessionID: "legacy-session", ExpectedRevision: 1, Prompt: "next"})
				if err != nil {
					t.Fatal(err)
				}
				if _, ok, err := store.LeaseNextTurn(context.Background(), "legacy-session"); err != nil || !ok {
					t.Fatal("lease", err)
				}
				if _, err = store.MarkTurnSendIntent(context.Background(), "legacy-session", turn.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = store.MarkTurnSent(context.Background(), "legacy-session", turn.ID); err != nil {
					t.Fatal(err)
				}
				turn, err = store.CompleteTurn(context.Background(), CompleteTurnRequest{SessionID: "legacy-session", TurnID: turn.ID, Message: "done", CumulativeCostUSD: 2, CostRecorded: true})
				if err != nil || turn.Usage.CostUSD != 2 {
					t.Fatalf("new provider cost: %+v %v", turn.Usage, err)
				}
			case "pending later turn":
				turn, err := store.GetTurn(context.Background(), "legacy-session", "pending")
				if err != nil || turn.Usage.CostUSD != 2 || cursor != 2 || !known {
					t.Fatalf("new epoch pending cost: %+v cursor%v/%v %v", turn.Usage, cursor, known, err)
				}
			default:
				if cursor != 1 || !known {
					t.Fatalf("unprovable historical cursor rewritten: %v %v", cursor, known)
				}
				turn, err := store.GetTurn(context.Background(), "legacy-session", "turn-legacy")
				if err != nil || turn.Usage.CostUSD != 0.25 {
					t.Fatal("historical turn changed", err)
				}
			}
		})
	}
}
