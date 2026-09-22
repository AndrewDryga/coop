// Command mcpprobe is a minimal stdio MCP server that records what a client actually did to it.
//
// It exists so a conformance probe can tell apart states that look identical from a config file: a
// client that never launched the server, one that launched it and never spoke, one that fired the
// handshake without waiting, and one that actually agreed a protocol. Every request it receives is
// appended to the file named by COOP_PROBE_LOG, one JSON-RPC method per line.
//
// Two details carry the weight. It records arrivals while delaying its initialize response, so
// pipelined notifications land BEFORE `answered initialize`. This witnesses receipt versus response
// order, not whether the client read the response. And it always
// touches $HOME/.mcpprobe-launched, whatever the log setting, so a client that launches the server
// but drops the configured env cannot be mistaken for one that never launched it at all.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// answerDelay is how long the server sits on its initialize RESULT. Long enough that a client which
// does not wait for it has already sent its notification; short enough to be invisible otherwise.
const answerDelay = 500 * time.Millisecond

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name string `json:"name"`
	} `json:"params"`
}

func (r request) event() string {
	if r.Method == "tools/call" {
		if r.Params.Name == "coop_probe_tool" {
			return "tools/call coop_probe_tool"
		}
		return "tools/call unexpected"
	}
	return r.Method
}

func main() {
	if home := os.Getenv("HOME"); home != "" {
		if f, err := os.Create(filepath.Join(home, ".mcpprobe-launched")); err == nil {
			f.Close()
		}
	}
	logPath := os.Getenv("COOP_PROBE_LOG")
	var recordMu sync.Mutex
	// Call with recordMu held. Response flush and its witness share the lock so a fast client's
	// next message cannot race ahead of the response it just received.
	record := func(event string) {
		if logPath == "" {
			return
		}
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintln(f, event)
	}
	record("launched")

	requests := make(chan request)
	go func() {
		defer close(requests)
		in := bufio.NewScanner(os.Stdin)
		in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for in.Scan() {
			var req request
			if err := json.Unmarshal(in.Bytes(), &req); err != nil {
				continue
			}
			recordMu.Lock()
			record(req.event())
			recordMu.Unlock()
			requests <- req
		}
	}()
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for req := range requests {
		if len(req.ID) == 0 {
			continue // a notification takes no response
		}
		var result any
		var rpcError any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "coop-probe", "version": "1.0.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name":        "coop_probe_tool",
				"description": "Report that the shared MCP server was reached.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			}}}
		case "resources/list":
			result = map[string]any{"resources": []any{}}
		case "prompts/list":
			result = map[string]any{"prompts": []any{}}
		case "tools/call":
			if req.Params.Name != "coop_probe_tool" {
				rpcError = map[string]any{"code": -32602, "message": "Unknown probe tool"}
			} else {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "shared MCP tool reached"}}}
			}
		default:
			result = map[string]any{}
		}
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcError != nil {
			response["error"] = rpcError
		} else {
			response["result"] = result
		}
		body, err := json.Marshal(response)
		if err != nil {
			continue
		}
		if req.Method == "initialize" {
			time.Sleep(answerDelay)
		}
		recordMu.Lock()
		_, _ = out.Write(body)
		_ = out.WriteByte('\n')
		if err := out.Flush(); err != nil {
			recordMu.Unlock()
			return
		}
		if req.Method == "initialize" {
			record("answered initialize")
		} else if req.Method == "tools/call" && rpcError == nil {
			record("answered tools/call coop_probe_tool")
		}
		recordMu.Unlock()
	}
}
