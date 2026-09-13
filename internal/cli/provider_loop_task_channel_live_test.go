//go:build providerlivee2e

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AndrewDryga/coop/internal/taskmcp"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// Observe actual task-tool replies, not provider narration or a folder it
// could move itself. The production server still owns every tool operation.
type providerLoopLiveTaskServer struct {
	server                                *taskmcp.Server
	root, id                              string
	refused                               atomic.Bool
	completed                             atomic.Bool
	invalid                               atomic.Bool
	cancel                                context.CancelFunc
	statsMu                               sync.Mutex
	stats                                 providerLoopLiveTaskObservation
	stateNeedsRepair, proposalNeedsRepair bool
}

func newProviderLoopLiveTaskServer(repo, provider string) (*providerLoopLiveTaskServer, error) {
	root, id := filepath.Join(repo, tasksRoot), providerLoopLiveTaskID(provider)
	// This diagnostic owns no loop iteration/ref window. It proves native
	// checklist-tool behavior; external controller/binding proof is separate.
	server, err := taskmcp.New(taskmcp.Authority{QueueRoots: []string{root}, Assigned: id})
	if err != nil {
		return nil, err
	}
	return &providerLoopLiveTaskServer{server: server, root: root, id: id, cancel: func() {}}, nil
}

func (s *providerLoopLiveTaskServer) Serve(ctx context.Context, rw io.ReadWriter) error {
	return s.server.Serve(ctx, &providerLoopLiveObservedConn{ReadWriter: rw, owner: s, pending: make(map[string]providerLoopLivePendingCall)})
}

func (s *providerLoopLiveTaskServer) verified() bool {
	return s.observation().verified()
}

type providerLoopLiveObservedConn struct {
	io.ReadWriter
	owner   *providerLoopLiveTaskServer
	partial []byte
	pending map[string]providerLoopLivePendingCall
}

type providerLoopLivePendingCall struct {
	name          string
	assignedState bool
}

func (c *providerLoopLiveObservedConn) Read(p []byte) (int, error) {
	n, err := c.ReadWriter.Read(p)
	if len(c.partial)+n > 4<<20 {
		c.owner.invalid.Store(true)
		c.partial = nil
		return n, err
	}
	c.partial = append(c.partial, p[:n]...)
	for {
		end := bytes.IndexByte(c.partial, '\n')
		if end < 0 {
			break
		}
		var request struct {
			ID     json.RawMessage
			Method string
			Params struct {
				Name      string
				Arguments json.RawMessage
			}
		}
		if json.Unmarshal(c.partial[:end], &request) == nil && request.Method == "tools/call" {
			if !c.owner.observeCall(request.Params.Name) {
				return 0, errors.New("live task-tool call limit exceeded")
			}
			if len(c.pending) >= 64 {
				c.owner.invalid.Store(true)
			} else {
				call := providerLoopLivePendingCall{name: request.Params.Name}
				if call.name == "tasks_update_state" {
					var args struct{ ID string }
					call.assignedState = json.Unmarshal(request.Params.Arguments, &args) == nil && args.ID == c.owner.id
				}
				c.pending[string(request.ID)] = call
			}
		}
		c.partial = c.partial[end+1:]
	}
	return n, err
}

func (c *providerLoopLiveObservedConn) Write(p []byte) (int, error) {
	n, err := c.ReadWriter.Write(p)
	if err != nil || n != len(p) {
		return n, err
	}
	var reply struct {
		ID     json.RawMessage
		Result struct {
			IsError bool
			Content []struct{ Text string }
		}
	}
	if json.Unmarshal(p, &reply) != nil {
		return n, err
	}
	call, pending := c.pending[string(reply.ID)]
	if !pending {
		return n, err
	}
	delete(c.pending, string(reply.ID))
	if len(reply.Result.Content) != 1 {
		c.owner.invalid.Store(true)
		return n, err
	}
	text := reply.Result.Content[0].Text
	name := call.name
	if name == "tasks_update_state" && !reply.Result.IsError && !call.assignedState {
		c.owner.invalid.Store(true)
	}
	c.owner.observeReply(name, text, reply.Result.IsError)
	if name != "tasks_complete" {
		return n, err
	}
	if reply.Result.IsError && strings.HasPrefix(text, "task checklist is unfinished: "+c.owner.id+" has ") {
		current, ok, readErr := tasks.CurrentTask(c.owner.root, c.owner.id)
		if readErr == nil && ok && current.State == tasks.StateInProgress && errors.Is(tasks.RequireCompletedChecklist(current), tasks.ErrIncompleteChecklist) {
			c.owner.refused.Store(true)
		}
	}
	if !reply.Result.IsError && strings.HasPrefix(text, c.owner.id+" moved to "+tasks.StateDone+"/") {
		if !c.owner.refused.Load() || c.owner.completed.Load() {
			c.owner.invalid.Store(true)
		} else {
			c.owner.completed.Store(true)
		}
	}
	return n, err
}

type providerLoopLiveTestConnection struct {
	io.Reader
	io.Writer
}

func TestProviderLoopLiveContractTaskChannel(t *testing.T) {
	for _, tc := range []struct {
		name         string
		firstRefusal bool
		wrongState   bool
	}{
		{"premature completion", false, false},
		{"assigned state", true, false},
		{"wrong task state", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			s, err := newProviderLoopLiveTaskServer(repo, "claude")
			if err != nil {
				t.Fatal(err)
			}
			writeTaskFile(t, filepath.Join(s.root, tasks.StateInProgress, s.id, "task.md"), "# Task\n\n## Subtasks\n- [ ] required check\n")
			var requests strings.Builder
			write := func(id int, name string, args any) {
				t.Helper()
				if err := json.NewEncoder(&requests).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.firstRefusal {
				write(1, "tasks_complete", map[string]any{"id": s.id})
			}
			stateID := s.id
			if tc.wrongState {
				stateID = "other-task"
				writeTaskFile(t, filepath.Join(s.root, tasks.StateTodo, stateID, "task.md"), "# Other task\n\n## Subtasks\n- [ ] check\n")
			}
			write(10, "tasks_update_state", map[string]any{"id": stateID, "status": "in progress", "done_so_far": "—", "next_action": "Run the check", "traps": "—"})
			proposal := providerLoopLiveProposal()
			write(11, "tasks_propose", map[string]any{"kind": proposal.Kind, "title": proposal.Title, "context": proposal.Context, "acceptance": proposal.Acceptance, "approach": proposal.Approach, "subtasks": proposal.Subtasks})
			write(2, "tasks_set_subtasks", map[string]any{"id": s.id, "subtasks": []map[string]any{{"text": "required check", "done": true}}})
			write(3, "tasks_complete", map[string]any{"id": s.id})
			var replies bytes.Buffer
			if err := s.Serve(context.Background(), providerLoopLiveTestConnection{strings.NewReader(requests.String()), &replies}); err != nil {
				t.Fatal(err)
			}
			want := tc.firstRefusal && !tc.wrongState
			if s.verified() != want {
				t.Fatalf("verified=%v want=%v; replies=%s", s.verified(), want, replies.String())
			}
			if !tc.firstRefusal {
				// A later refusal/repair cannot erase premature success, even
				// if the provider can reopen its writable fixture folder.
				done, ok, err := tasks.CurrentTask(s.root, s.id)
				if err != nil || !ok {
					t.Fatal(err)
				}
				if err := tasks.MoveTaskDir(s.root, done, tasks.StateInProgress); err != nil {
					t.Fatal(err)
				}
				if err := tasks.RewriteSubtasks(filepath.Join(s.root, tasks.StateInProgress, s.id), []tasks.Subtask{{Text: "required check"}}); err != nil {
					t.Fatal(err)
				}
				requests.Reset()
				write(4, "tasks_complete", map[string]any{"id": s.id})
				write(5, "tasks_set_subtasks", map[string]any{"id": s.id, "subtasks": []map[string]any{{"text": "required check", "done": true}}})
				write(6, "tasks_complete", map[string]any{"id": s.id})
				if err := s.Serve(context.Background(), providerLoopLiveTestConnection{strings.NewReader(requests.String()), &replies}); err != nil {
					t.Fatal(err)
				}
				if s.verified() {
					t.Fatal("premature completion was forgotten after a later refusal and repair")
				}
			}
		})
	}
}
