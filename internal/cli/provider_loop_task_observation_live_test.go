//go:build providerlivee2e

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

// A paid diagnostic has a small fixed job. This cap is test-only, never a limit on product work.
const providerLoopLiveTaskCallLimit = 32

type providerLoopLiveToolCounts struct {
	Calls         int `json:"calls"`
	Accepted      int `json:"accepted"`
	MissingFields int `json:"missing_fields"`
	OtherRefusals int `json:"other_refusals"`
	Repairs       int `json:"repairs"`
}

type providerLoopLiveTaskObservation struct {
	Calls            int                          `json:"calls"`
	State            providerLoopLiveToolCounts   `json:"state"`
	Proposal         providerLoopLiveToolCounts   `json:"proposal"`
	Search           providerLoopLiveSearchCounts `json:"search"`
	OtherRefusals    int                          `json:"other_refusals"`
	ChecklistRefused bool                         `json:"checklist_refused"`
	Completed        bool                         `json:"completed"`
	Invalid          bool                         `json:"invalid"`
	LimitExceeded    bool                         `json:"limit_exceeded"`
}

func (s *providerLoopLiveTaskServer) observeCall(name string) bool {
	s.statsMu.Lock()
	if s.stats.LimitExceeded {
		s.statsMu.Unlock()
		return false
	}
	s.stats.Calls++
	switch name {
	case "tasks_update_state":
		s.stats.State.Calls++
	case "tasks_propose":
		s.stats.Proposal.Calls++
	}
	exceeded := s.stats.Calls > providerLoopLiveTaskCallLimit
	if exceeded {
		s.stats.LimitExceeded = true
		s.invalid.Store(true)
	}
	s.statsMu.Unlock()
	if exceeded {
		s.cancel()
	}
	return !exceeded
}

func (s *providerLoopLiveTaskServer) observeReply(name, text string, refused bool) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	var counts *providerLoopLiveToolCounts
	var needsRepair *bool
	switch name {
	case "tasks_update_state":
		counts, needsRepair = &s.stats.State, &s.stateNeedsRepair
	case "tasks_propose":
		counts, needsRepair = &s.stats.Proposal, &s.proposalNeedsRepair
	default:
		if refused && !(name == "tasks_complete" && strings.HasPrefix(text, "task checklist is unfinished: "+s.id+" has ")) {
			s.stats.OtherRefusals++
		}
		return
	}
	if !refused {
		counts.Accepted++
		if *needsRepair {
			counts.Repairs++
			*needsRepair = false
		}
	} else if strings.HasPrefix(text, "missing required fields: ") {
		counts.MissingFields++
		*needsRepair = true
	} else {
		counts.OtherRefusals++
	}
}

func (s *providerLoopLiveTaskServer) observation() providerLoopLiveTaskObservation {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	out := s.stats
	out.ChecklistRefused, out.Completed, out.Invalid = s.refused.Load(), s.completed.Load(), s.invalid.Load()
	return out
}

func (o providerLoopLiveTaskObservation) valid() bool {
	if o.Calls < 0 || o.Calls > providerLoopLiveTaskCallLimit+1 || o.OtherRefusals < 0 || o.OtherRefusals > o.Calls ||
		o.LimitExceeded != (o.Calls > providerLoopLiveTaskCallLimit) || o.State.Calls+o.Proposal.Calls+o.Search.Calls > o.Calls || !o.Search.valid(o.Calls) {
		return false
	}
	for _, c := range []providerLoopLiveToolCounts{o.State, o.Proposal} {
		if c.Calls < 0 || c.Calls > o.Calls || c.Accepted < 0 || c.MissingFields < 0 || c.OtherRefusals < 0 || c.Repairs < 0 ||
			c.Accepted > c.Calls || c.MissingFields > c.Calls || c.OtherRefusals > c.Calls || c.Repairs > c.Calls ||
			c.Accepted+c.MissingFields+c.OtherRefusals > c.Calls || c.Repairs > c.Accepted || c.Repairs > c.MissingFields {
			return false
		}
	}
	return true
}

func (o providerLoopLiveTaskObservation) verified() bool {
	return o.valid() && o.ChecklistRefused && o.Completed && !o.Invalid && !o.LimitExceeded &&
		o.State.Accepted > 0 && o.Proposal.Accepted == 1 && o.Search.Accepted > 0
}

func (s *providerLoopLiveTaskServer) writeObservation(path string) error {
	data, err := json.Marshal(s.observation())
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readProviderLoopLiveObservation(layout procharness.Layout, path string) (providerLoopLiveTaskObservation, error) {
	data, err := readProviderLoopLiveFile(layout, path)
	trimmed := bytes.TrimSpace(data)
	if err != nil || len(trimmed) == 0 || len(data) > 4096 || trimmed[0] != '{' {
		return providerLoopLiveTaskObservation{}, errors.New("invalid live task-tool observation file")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var observation providerLoopLiveTaskObservation
	if err := decoder.Decode(&observation); err != nil {
		return observation, errors.New("invalid live task-tool observation")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) || !observation.valid() {
		return observation, errors.New("invalid live task-tool observation counts")
	}
	return observation, nil
}

func TestProviderLoopLiveContractCallCapSurvivesReconnects(t *testing.T) {
	repo := t.TempDir()
	s, err := newProviderLoopLiveTaskServer(repo, "claude")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.cancel = cancel
	for i := 0; i <= providerLoopLiveTaskCallLimit; i++ {
		request := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"tasks_list\",\"arguments\":{}}}\n"
		var replies bytes.Buffer
		err := s.Serve(ctx, providerLoopLiveTestConnection{strings.NewReader(request), &replies})
		if i < providerLoopLiveTaskCallLimit {
			if err != nil || ctx.Err() != nil {
				t.Fatalf("call %d failed before cap: %v", i+1, err)
			}
		} else if err == nil || ctx.Err() == nil || replies.Len() != 0 {
			t.Fatalf("overflow did not cancel/refuse: err=%v ctx=%v replies=%d", err, ctx.Err(), replies.Len())
		}
	}
	o := s.observation()
	if !o.valid() || !o.LimitExceeded || !o.Invalid || o.verified() {
		t.Fatalf("overflow observation = %+v", o)
	}
}

func TestProviderLoopLiveContractCountsOnlyMatchedInputRepairs(t *testing.T) {
	repo := t.TempDir()
	s, err := newProviderLoopLiveTaskServer(repo, "claude")
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(s.root, "10_in_progress", s.id, "task.md"), "# Task\n\n## Subtasks\n- [ ] check\n")
	var requests strings.Builder
	encoder := json.NewEncoder(&requests)
	for i, args := range []map[string]any{
		{"id": s.id, "status": "in progress", "done_so_far": "—"},
		{"id": s.id, "status": "in progress", "done_so_far": "—", "next_action": "Run check", "traps": "—"},
	} {
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": "tools/call", "params": map[string]any{"name": "tasks_update_state", "arguments": args}}); err != nil {
			t.Fatal(err)
		}
	}
	var replies bytes.Buffer
	if err := s.Serve(context.Background(), providerLoopLiveTestConnection{strings.NewReader(requests.String()), &replies}); err != nil {
		t.Fatal(err)
	}
	o := s.observation()
	if o.Calls != 2 || o.State != (providerLoopLiveToolCounts{Calls: 2, Accepted: 1, MissingFields: 1, Repairs: 1}) || o.verified() {
		t.Fatalf("matched state repair = %+v", o)
	}
}

func TestProviderLoopLiveContractObservationFilesAreBoundedAndTyped(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := newProviderLoopLiveTaskServer(layout.Repo, "claude")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(layout.State, "observed.json")
	if err := s.writeObservation(path); err != nil {
		t.Fatal(err)
	}
	if _, err := readProviderLoopLiveObservation(layout, path); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		"", " \n", "null", "{}", `{"calls":-1}`, `{"calls":33}`,
		`{"calls":1,"state":{"calls":1,"accepted":9223372036854775807}}`,
		`{"calls":1,"secret":"must not be retained"}`, "{} {}",
		strings.Repeat(" ", 4097),
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		o, err := readProviderLoopLiveObservation(layout, path)
		if data == "{}" {
			if err != nil || o.verified() {
				t.Fatalf("zero observation is not proof: %+v %v", o, err)
			}
		} else if err == nil {
			t.Fatalf("invalid observation accepted: %q", data)
		}
	}
}
