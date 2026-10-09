package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// TurnTokens are a turn's token counts as a client recorded them. Input includes the cached input,
// as Codex counts it.
type TurnTokens struct {
	Input      int `json:"input_tokens"`
	Cached     int `json:"cached_input_tokens"`
	CacheWrite int `json:"cache_write_input_tokens"`
	Output     int `json:"output_tokens"`
	Reasoning  int `json:"reasoning_output_tokens"`
}

// TurnRecord is an adapter whose ACP client answers a prompt with the usage of the turn's last
// model call, while the client's own session record holds every call of the turn.
type TurnRecord interface {
	// LastTurnTokens reads the selected complete native home, never an account's
	// legacy profile: the usage of its last task and that task's last call.
	LastTurnTokens(home, nativeID string) (whole, last TurnTokens, ok bool)
}

// codex-acp (1.10.0 and 2.0.0 alike) answers a prompt with the usage of the turn's last model call
// only. A turn that uses tools makes many calls, so a 23-call turn was recorded as 93k input and 737
// output tokens where it used 1.79M and 4,889 (2026-09-28, the live worker's rollouts). Codex writes
// each call's `token_count` to the session's rollout with a running total for the process, which
// restarts with each process and carries across the tasks one process serves. The turn is the
// rollout's last task: its last running total, less what the total already held before the task's
// first call.
func (codexAgent) LastTurnTokens(home, nativeID string) (TurnTokens, TurnTokens, bool) {
	path := codexRollout(home, nativeID)
	if path == "" {
		return TurnTokens{}, TurnTokens{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return TurnTokens{}, TurnTokens{}, false
	}
	defer file.Close()
	return codexRolloutTurn(io.LimitReader(file, 1<<30))
}

func (u TurnTokens) minus(other TurnTokens) TurnTokens {
	return TurnTokens{
		Input: u.Input - other.Input, Cached: u.Cached - other.Cached, CacheWrite: u.CacheWrite - other.CacheWrite,
		Output: u.Output - other.Output, Reasoning: u.Reasoning - other.Reasoning,
	}
}

func (u TurnTokens) plus(other TurnTokens) TurnTokens {
	return TurnTokens{
		Input: u.Input + other.Input, Cached: u.Cached + other.Cached, CacheWrite: u.CacheWrite + other.CacheWrite,
		Output: u.Output + other.Output, Reasoning: u.Reasoning + other.Reasoning,
	}
}

func (u TurnTokens) valid() bool {
	return u.Input >= 0 && u.Cached >= 0 && u.CacheWrite >= 0 && u.Output >= 0 && u.Reasoning >= 0 &&
		u.Cached <= u.Input
}

// codexRolloutTurn reads a rollout and returns the usage of its last task and of that task's last
// model call.
func codexRolloutTurn(reader io.Reader) (whole, last TurnTokens, ok bool) {
	var first, final *struct {
		Total TurnTokens `json:"total_token_usage"`
		Last  TurnTokens `json:"last_token_usage"`
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
						Total TurnTokens `json:"total_token_usage"`
						Last  TurnTokens `json:"last_token_usage"`
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
		return TurnTokens{}, TurnTokens{}, false
	}
	whole = final.Total.minus(first.Total).plus(first.Last)
	if !whole.valid() || !final.Last.valid() {
		return TurnTokens{}, TurnTokens{}, false
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

// codexRollout is the newest rollout of one native session in its complete home, or
// "" when there is none.
func codexRollout(home, nativeID string) string {
	if home == "" || !filepath.IsAbs(home) || !codexPathComponent(nativeID) {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+nativeID+".jsonl"))
	if err != nil {
		return ""
	}
	archived, err := filepath.Glob(filepath.Join(home, "archived_sessions", "rollout-*-"+nativeID+".jsonl"))
	if err != nil {
		return ""
	}
	matches = append(matches, archived...)
	if len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}

func codexPathComponent(value string) bool {
	return value != "" && len(value) <= 256 && value != "." && value != ".." &&
		!strings.ContainsAny(value, "/\\*?[\x00")
}
