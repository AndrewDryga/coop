// Command mcpprobe is a minimal stdio MCP server that records what a client actually did to it.
//
// It exists so a conformance probe can tell apart states that look identical from a config file: a
// client that never launched the server, one that launched it and never spoke, one that fired the
// handshake without waiting, and one that actually agreed a protocol. Every request it receives is
// appended to the file named by COOP_PROBE_LOG, one JSON-RPC method per line.
//
// Two details carry the weight. It records `answered initialize` AFTER writing that response, and it
// waits before writing it — so a client that sends `notifications/initialized` without reading the
// result lands BEFORE that line, and order alone separates "agreed" from "announced". And it always
// touches $HOME/.mcpprobe-launched, whatever the log setting, so a client that launches the server
// but drops the configured env cannot be mistaken for one that never launched it at all.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// answerDelay is how long the server sits on its initialize RESULT. Long enough that a client which
// does not wait for it has already sent its notification; short enough to be invisible otherwise.
const answerDelay = 500 * time.Millisecond

func main() {
	if home := os.Getenv("HOME"); home != "" {
		if f, err := os.Create(filepath.Join(home, ".mcpprobe-launched")); err == nil {
			f.Close()
		}
	}
	logPath := os.Getenv("COOP_PROBE_LOG")
	record := func(event string) {
		if logPath == "" {
			return
		}
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintln(f, event)
	}
	record("launched")

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for in.Scan() {
		line := in.Bytes()
		if len(line) == 0 {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		record(req.Method)
		if len(req.ID) == 0 {
			continue // a notification takes no response
		}
		var result any
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
		default:
			result = map[string]any{}
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if err != nil {
			continue
		}
		if req.Method == "initialize" {
			time.Sleep(answerDelay)
		}
		out.Write(body)
		out.WriteByte('\n')
		out.Flush()
		if req.Method == "initialize" {
			record("answered initialize")
		}
	}
}
