// Package taskmcp is the coop-owned MCP server an in-box agent reaches its task queue through.
// It speaks newline-delimited JSON-RPC 2.0 (initialize, tools/list, tools/call) over any
// io.ReadWriter — hand-rolled like internal/acpproxy, no SDK — and exposes exactly eight typed
// task tools over internal/tasks, so the box never needs the coop binary, a docker socket, or a
// network route to change task state.
//
// The server is built host-side from an explicit Authority — never ambient config — and every
// tool works on every task in those queues. Lease refusals protect tasks another live process
// holds. Assigned completion also has a pre-move commit check for repairable feedback; the
// post-exit audit remains authoritative.
package taskmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/AndrewDryga/coop/internal/mcp"
	"github.com/AndrewDryga/coop/internal/tasks"
)

const (
	// ServerName is the reserved MCP server name every provider projection sees; operator config
	// cannot shadow it (mcp.BindTaskTools).
	ServerName = mcp.TaskToolsServer

	// maxFrameBytes bounds one JSON-RPC line, matching mcp.go's 4 MiB cap on a config it reads.
	// A larger frame is refused as a parse error and skipped; the session goes on.
	maxFrameBytes = 4 << 20

	protocolVersion = "2025-06-18"
	serverVersion   = "1"
)

// supportedProtocolVersions are the MCP revisions this server answers; a client's own revision is
// echoed back when it is one of them, so no client is told to speak a newer wire than it offered.
var supportedProtocolVersions = map[string]bool{"2024-11-05": true, "2025-03-26": true, protocolVersion: true}

// Authority is everything the server may act on, assembled by the host process that owns the
// queue. Nothing here is read from the environment or a config file.
type Authority struct {
	// QueueRoots are the absolute task queue roots the tools reach — every task in them.
	QueueRoots []string
	// Assigned is the task the launching iteration already leases host-side (the loop's
	// AssignLoopTaskOnly). Mutations on it run under that lease; the server never takes a second
	// one on the same id. Empty when nothing is assigned: then every mutation leases for itself.
	Assigned string
	// ValidateAssignedCompletion checks the launching iteration's immutable commit boundary
	// before the assigned folder moves. It gives the live agent a chance to repair a missing
	// commit; the host's complete post-exit audit still decides whether completion is accepted.
	// Nil for standalone task-channel probes with no Git completion contract.
	ValidateAssignedCompletion func() error
	// ProposalOutbox, when set, is where tasks_propose writes validated proposals instead of
	// creating queue folders: a fork's one-task projection has no canonical todo/backlog, so the
	// host imports the outbox when the fork lands (tasks.ImportForkProposals).
	ProposalOutbox string
	// Owner identifies the leases the server takes on tasks other than Assigned.
	Owner tasks.TaskLeaseOwner
}

// Server serves task tools for one Authority to any number of sessions.
type Server struct {
	authority Authority
	// mu serializes tool execution across sessions: the lead and its peers share one queue, and
	// every mutation is a read-check-write on the filesystem.
	mu sync.Mutex
}

// New validates the authority and returns a server for it.
func New(authority Authority) (*Server, error) {
	if len(authority.QueueRoots) == 0 {
		return nil, errors.New("task tools need at least one queue root")
	}
	return &Server{authority: authority}, nil
}

// Authority returns the authority the server was built from.
func (s *Server) Authority() Authority { return s.authority }

// Serve runs one MCP session over rw until the peer closes it or ctx ends. Every request gets a
// reply; a notification never does. Malformed JSON answers -32700, an unknown method -32601, and an
// oversized frame is refused and skipped — the session continues.
func (s *Server) Serve(ctx context.Context, rw io.ReadWriter) error {
	reader := bufio.NewReaderSize(rw, 64<<10)
	var writeMu sync.Mutex
	write := func(frame any) error {
		data, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err = rw.Write(append(data, '\n'))
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := readFrame(reader)
		if errors.Is(err, errFrameTooLarge) {
			if werr := write(errorResponse(nil, codeParse, "frame exceeds the 4 MiB limit")); werr != nil {
				return werr
			}
			continue
		}
		if len(line) > 0 {
			if reply := s.handle(ctx, line); reply != nil {
				if werr := write(reply); werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

var errFrameTooLarge = errors.New("frame too large")

// readFrame returns the next newline-terminated frame without its terminator. A frame over
// maxFrameBytes is drained to its newline and reported as errFrameTooLarge so the caller can
// refuse it and keep the session.
func readFrame(reader *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(frame)+len(chunk) > maxFrameBytes {
			// Drain the rest of this line, then refuse it.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = reader.ReadSlice('\n')
			}
			if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
				return nil, errors.Join(errFrameTooLarge, err)
			}
			return nil, errFrameTooLarge
		}
		frame = append(frame, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return bytes.TrimRight(frame, "\r\n"), err
		}
		return bytes.TrimRight(frame, "\r\n"), nil
	}
}

// JSON-RPC 2.0 error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func errorResponse(id json.RawMessage, code int, message string) *response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func resultResponse(id json.RawMessage, result any) *response {
	return &response{JSONRPC: "2.0", ID: id, Result: result}
}

// handle answers one frame; nil means "no reply" (a notification).
func (s *Server) handle(ctx context.Context, line []byte) *response {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(nil, codeParse, "malformed JSON: "+err.Error())
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if len(req.ID) == 0 {
			return nil
		}
		return errorResponse(req.ID, codeInvalidRequest, "not a JSON-RPC 2.0 request")
	}
	notification := len(req.ID) == 0 || bytes.Equal(req.ID, []byte("null"))
	if notification {
		// notifications/initialized, notifications/cancelled, and anything else fire-and-forget.
		return nil
	}
	if !validID(req.ID) {
		return errorResponse(nil, codeInvalidRequest, "request id must be a string or a number")
	}
	switch req.Method {
	case "initialize":
		return resultResponse(req.ID, s.initialize(req.Params))
	case "ping":
		return resultResponse(req.ID, map[string]any{})
	case "tools/list":
		return resultResponse(req.ID, map[string]any{"tools": toolDescriptors()})
	case "tools/call":
		result, err := s.call(ctx, req.Params)
		if err != nil {
			var rerr *rpcError
			if errors.As(err, &rerr) {
				return errorResponse(req.ID, rerr.Code, rerr.Message)
			}
			return errorResponse(req.ID, codeInternal, err.Error())
		}
		return resultResponse(req.ID, result)
	default:
		return errorResponse(req.ID, codeMethodNotFound, fmt.Sprintf("method %q is not served here — this socket answers the coop task tools only", req.Method))
	}
}

func (e *rpcError) Error() string { return e.Message }

func validID(id json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(id, &v); err != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

func (s *Server) initialize(params json.RawMessage) map[string]any {
	version := protocolVersion
	var offered struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &offered) == nil && supportedProtocolVersions[offered.ProtocolVersion] {
		version = offered.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": ServerName, "version": serverVersion},
		"instructions":    serverInstructions,
	}
}

// serverInstructions is what a client shows its model about this server.
const serverInstructions = "coop's task queue for this box. tasks_list / tasks_get read any task; tasks_update_state, tasks_append_log, tasks_set_subtasks keep the assigned task's files current; tasks_complete and tasks_block move it out of 10_in_progress; tasks_propose files newly discovered work. A mutation on a task another live process holds is refused."

// toolResult is the MCP tools/call result shape: text content, flagged as an error when the tool
// refused or failed so the model reads the reason instead of a protocol fault.
type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textResult(text string) *toolResult {
	return &toolResult{Content: []toolContent{{Type: "text", Text: text}}}
}

func jsonResult(v any) *toolResult {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return refusal("encode result: " + err.Error())
	}
	return textResult(string(data))
}

func refusal(text string) *toolResult {
	return &toolResult{Content: []toolContent{{Type: "text", Text: text}}, IsError: true}
}

func (s *Server) call(ctx context.Context, params json.RawMessage) (*toolResult, error) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call needs a tool name"}
	}
	t, ok := toolByName(call.Name)
	if !ok {
		return nil, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("unknown tool %q — this socket serves only: %s", call.Name, toolNames())}
	}
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage("{}")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := missingRequiredArguments(t, call.Arguments); r != nil {
		return r, nil
	}
	return t.run(s, ctx, call.Arguments), nil
}
