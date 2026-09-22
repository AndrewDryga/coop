package box

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMCPProbeWitness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "mcpprobe")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "./testdata/mcpprobe").CombinedOutput(); err != nil {
		t.Fatalf("build witness: %v\n%s", err, out)
	}
	for _, pipelined := range []bool{false, true} {
		t.Run(fmt.Sprintf("pipelined=%v", pipelined), func(t *testing.T) {
			root := t.TempDir()
			logPath := filepath.Join(root, "witness")
			cmd := exec.CommandContext(ctx, binary)
			cmd.Env = append(os.Environ(), "HOME="+root, "COOP_PROBE_LOG="+logPath)
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
			scanner := bufio.NewScanner(output)
			read := func() map[string]json.RawMessage {
				t.Helper()
				if !scanner.Scan() {
					t.Fatalf("missing probe response: %v", scanner.Err())
				}
				var response map[string]json.RawMessage
				if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				return response
			}
			write := func(line string) {
				t.Helper()
				if _, err := fmt.Fprintln(input, line); err != nil {
					t.Fatal(err)
				}
			}
			const initialized = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
			const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
			if pipelined {
				write(initialize + "\n" + initialized)
			} else {
				write(initialize)
			}
			read()
			if !pipelined {
				write(initialized)
			}
			write(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
			read()
			write(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"coop_probe_tool"}}`)
			response := read()
			if !strings.Contains(string(response["result"]), "shared MCP tool reached") {
				t.Error("probe did not return the tool result")
			}
			write(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"UNTRUSTED_NAME_CANARY"}}`)
			if response := read(); len(response["error"]) == 0 || len(response["result"]) != 0 {
				t.Error("unknown tool did not receive a JSON-RPC error")
			}
			if err := input.Close(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			answer, notification := slices.Index(lines, "answered initialize"), slices.Index(lines, "notifications/initialized")
			if answer < 0 || notification < 0 || (notification < answer) != pipelined {
				t.Errorf("arrival order does not distinguish pipelining: %q", lines)
			}
			if strings.Contains(string(data), "UNTRUSTED_NAME_CANARY") {
				t.Error("witness retained model-controlled tool name")
			}
		})
	}
}
