package sessionsvc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
)

// A Codex turn's usage, counted from Codex's own record of it.
//
// codex-acp (1.10.0 and 2.0.0 alike) answers a prompt with the usage of the
// turn's last model call only. A turn that uses tools makes many calls, so a
// 23-call turn was recorded as 93k input and 737 output tokens where it used
// 1.79M and 4,889 (2026-09-28, the live worker's rollouts). Codex writes each
// call's `token_count` to the session's rollout with a running total for the
// process, which restarts with each process and carries across the tasks one
// process serves. The turn is the rollout's last task: its last running total,
// less what the total already held before the task's first call.
const (
	codexRolloutWait      = 100 * time.Millisecond
	codexRolloutAttempts  = 10
	codexRolloutProvider  = "codex"
	codexRolloutSubfolder = "sessions"
)

type codexTokenUsage struct {
	Input      int `json:"input_tokens"`
	Cached     int `json:"cached_input_tokens"`
	CacheWrite int `json:"cache_write_input_tokens"`
	Output     int `json:"output_tokens"`
	Reasoning  int `json:"reasoning_output_tokens"`
}

func (u codexTokenUsage) minus(other codexTokenUsage) codexTokenUsage {
	return codexTokenUsage{
		Input: u.Input - other.Input, Cached: u.Cached - other.Cached, CacheWrite: u.CacheWrite - other.CacheWrite,
		Output: u.Output - other.Output, Reasoning: u.Reasoning - other.Reasoning,
	}
}

func (u codexTokenUsage) plus(other codexTokenUsage) codexTokenUsage {
	return codexTokenUsage{
		Input: u.Input + other.Input, Cached: u.Cached + other.Cached, CacheWrite: u.CacheWrite + other.CacheWrite,
		Output: u.Output + other.Output, Reasoning: u.Reasoning + other.Reasoning,
	}
}

func (u codexTokenUsage) valid() bool {
	return u.Input >= 0 && u.Cached >= 0 && u.CacheWrite >= 0 && u.Output >= 0 && u.Reasoning >= 0 &&
		u.Cached <= u.Input
}

// session maps Codex's counters the way codex-acp does: its input includes the
// cached input, which session.Usage keeps apart because providers price it
// differently.
func (u codexTokenUsage) session() session.Usage {
	return session.Usage{
		InputTokens:       u.Input - u.Cached,
		CachedInputTokens: u.Cached + u.CacheWrite,
		OutputTokens:      u.Output,
		ReasoningTokens:   u.Reasoning,
	}
}

// codexRolloutTurn reads a rollout and returns the usage of its last task and
// of that task's last model call.
func codexRolloutTurn(reader io.Reader) (whole, last codexTokenUsage, ok bool) {
	var first, final *struct {
		Total codexTokenUsage `json:"total_token_usage"`
		Last  codexTokenUsage `json:"last_token_usage"`
	}
	lines := bufio.NewReaderSize(reader, 64<<10)
	for {
		line, err := lines.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// A long tool output or message: skip the rest of the line.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = lines.ReadSlice('\n')
			}
			line = nil
		}
		switch {
		case bytes.Contains(line, []byte(`"task_started"`)):
			if codexRolloutEvent(line) == "task_started" {
				first, final = nil, nil
			}
		case bytes.Contains(line, []byte(`"token_count"`)):
			var event struct {
				Type    string `json:"type"`
				Payload struct {
					Type string `json:"type"`
					Info *struct {
						Total codexTokenUsage `json:"total_token_usage"`
						Last  codexTokenUsage `json:"last_token_usage"`
					} `json:"info"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &event) == nil && event.Type == "event_msg" &&
				event.Payload.Type == "token_count" && event.Payload.Info != nil {
				if first == nil {
					first = event.Payload.Info
				}
				final = event.Payload.Info
			}
		}
		if err != nil {
			break
		}
	}
	if first == nil || final == nil {
		return codexTokenUsage{}, codexTokenUsage{}, false
	}
	whole = final.Total.minus(first.Total).plus(first.Last)
	if !whole.valid() || !final.Last.valid() {
		return codexTokenUsage{}, codexTokenUsage{}, false
	}
	return whole, final.Last, true
}

func codexRolloutEvent(line []byte) string {
	var event struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &event) != nil || event.Type != "event_msg" {
		return ""
	}
	return event.Payload.Type
}

// codexRollout is the newest rollout of one native session under a Codex
// profile, or "" when there is none.
func codexRollout(profile, nativeID string) string {
	if profile == "" || !validSessionPathComponent(nativeID) {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(profile, codexRolloutSubfolder, "*", "*", "*", "rollout-*-"+nativeID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}

// codexWholeTurnUsage is the turn's usage from its rollout when the rollout
// holds the call codex-acp reported last, the proof that Codex has written the
// whole turn; `reported` stands otherwise. The rollout may lag the adapter's
// answer by a moment, so it is read a few times first.
func codexWholeTurnUsage(ctx context.Context, profile, nativeID string, reported session.Usage) session.Usage {
	path := codexRollout(profile, nativeID)
	if path == "" || !reported.Recorded() {
		return reported
	}
	for attempt := 0; attempt < codexRolloutAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return reported
			case <-time.After(codexRolloutWait):
			}
		}
		file, err := os.Open(path)
		if err != nil {
			return reported
		}
		whole, last, ok := codexRolloutTurn(io.LimitReader(file, 1<<30))
		file.Close()
		if ok && sameCodexCall(last, reported) {
			usage := whole.session()
			usage.CostUSD, usage.CostRecorded = reported.CostUSD, reported.CostRecorded
			return usage
		}
	}
	return reported
}

// sameCodexCall compares a rollout's call with the one codex-acp reported:
// its whole input, cached reads included, and its output.
func sameCodexCall(call codexTokenUsage, reported session.Usage) bool {
	return call.Input == reported.InputTokens+reported.CachedInputTokens && call.Output == reported.OutputTokens
}

// codexProfile is the private Codex home of a session's child, where its
// rollouts are written, or "" when it is not known.
func codexProfile(process *sessionACPProcess, account string) string {
	if process == nil || process.privateRoot == "" || !validSessionPathComponent(account) {
		return ""
	}
	return filepath.Join(process.privateRoot, codexRolloutProvider, "profiles", account)
}
