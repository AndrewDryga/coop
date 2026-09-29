package sessionsvc

import (
	"context"
	"sync/atomic"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/session"
)

// A record the client may still be writing: the turn's last call shows up only after `lag` reads.
type laggingTurnRecord struct {
	whole, last agents.TurnTokens
	lag         int32
	reads       atomic.Int32
}

func (r *laggingTurnRecord) LastTurnTokens(_, _, _ string) (agents.TurnTokens, agents.TurnTokens, bool) {
	if r.reads.Add(1) <= r.lag {
		return agents.TurnTokens{Input: 1, Output: 1}, agents.TurnTokens{Input: 1, Output: 1}, true
	}
	return r.whole, r.last, true
}

// codex-acp reported the live worker's 2026-09-28 06:31 task by its last call alone: 106,085 input
// (104,704 cached) and 358 output tokens, for a task of 1,020,300 and 1,900.
func TestATurnsUsageComesFromTheClientsRecordOnceItHoldsTheReportedCall(t *testing.T) {
	record := &laggingTurnRecord{
		whole: agents.TurnTokens{Input: 1_020_300, Cached: 1_005_440, Output: 1_900, Reasoning: 99},
		last:  agents.TurnTokens{Input: 106_085, Cached: 104_704, Output: 358},
		lag:   2,
	}
	reported := session.Usage{InputTokens: 106_085 - 104_704, CachedInputTokens: 104_704, OutputTokens: 358, CostUSD: 0.5, CostRecorded: true}

	got := wholeTurnUsage(context.Background(), record, "/private", "default", "native", reported)
	want := session.Usage{InputTokens: 1_020_300 - 1_005_440, CachedInputTokens: 1_005_440, OutputTokens: 1_900, ReasoningTokens: 99, CostUSD: 0.5, CostRecorded: true}
	if got != want {
		t.Fatalf("whole turn %+v, want %+v", got, want)
	}
	if reads := record.reads.Load(); reads != 3 {
		t.Fatalf("the record was read %d times, want it read until it caught up (3)", reads)
	}

	// A record that never holds the reported call is behind the adapter, or about another turn:
	// what the adapter reported stands.
	behind := &laggingTurnRecord{lag: 1 << 20}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := wholeTurnUsage(ctx, behind, "/private", "default", "native", reported); got != reported {
		t.Fatalf("an unconfirmed record replaced the report: %+v", got)
	}

	// No record, no private home, or nothing reported: the report stands.
	if got := wholeTurnUsage(context.Background(), nil, "/private", "default", "native", reported); got != reported {
		t.Fatalf("no record changed the report: %+v", got)
	}
	if got := wholeTurnUsage(context.Background(), record, "", "default", "native", reported); got != reported {
		t.Fatalf("no private home changed the report: %+v", got)
	}
	if got := wholeTurnUsage(context.Background(), record, "/private", "default", "native", session.Usage{}); got != (session.Usage{}) {
		t.Fatalf("an empty report became %+v", got)
	}
}
