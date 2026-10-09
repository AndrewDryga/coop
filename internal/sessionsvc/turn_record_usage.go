package sessionsvc

import (
	"context"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/session"
)

// A turn's usage, counted from the client's own record of it when its adapter keeps one
// (agents.TurnRecord). codex-acp answers a prompt with the usage of the turn's last model call only,
// so a 23-call turn was recorded as 93k input and 737 output tokens where it used 1.79M and 4,889
// (2026-09-28, the live worker's rollouts).
//
// The record is trusted only once it holds the call the adapter reported last, the proof that the
// client has written the whole turn; the adapter's own figure stands otherwise. The record may lag
// the adapter's answer by a moment, so it is read a few times first.
const (
	turnRecordWait     = 100 * time.Millisecond
	turnRecordAttempts = 10
)

func turnRecordOf(provider string) (agents.TurnRecord, bool) {
	agent, ok := agents.Get(provider)
	if !ok {
		return nil, false
	}
	record, ok := agent.(agents.TurnRecord)
	return record, ok
}

func wholeTurnUsage(ctx context.Context, record agents.TurnRecord, home, nativeID string, reported session.Usage) session.Usage {
	if record == nil || home == "" || !reported.Recorded() {
		return reported
	}
	for attempt := 0; attempt < turnRecordAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return reported
			case <-time.After(turnRecordWait):
			}
		}
		whole, last, ok := record.LastTurnTokens(home, nativeID)
		if ok && sameTurnCall(last, reported) {
			usage := turnTokensUsage(whole)
			usage.CostUSD, usage.CostRecorded = reported.CostUSD, reported.CostRecorded
			return usage
		}
	}
	return reported
}

// sameTurnCall compares the record's last call with the one the adapter reported: its whole input,
// cached reads included, and its output.
func sameTurnCall(call agents.TurnTokens, reported session.Usage) bool {
	return call.Input == reported.InputTokens+reported.CachedInputTokens && call.Output == reported.OutputTokens
}

// turnTokensUsage maps a record's counters the way the adapters do: the record's input includes
// the cached input, which session.Usage keeps apart because providers price it differently.
func turnTokensUsage(tokens agents.TurnTokens) session.Usage {
	return session.Usage{
		InputTokens:       tokens.Input - tokens.Cached,
		CachedInputTokens: tokens.Cached + tokens.CacheWrite,
		OutputTokens:      tokens.Output,
		ReasoningTokens:   tokens.Reasoning,
	}
}
