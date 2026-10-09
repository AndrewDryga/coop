package acpproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestProjectSessionMCP(t *testing.T) {
	shared := []map[string]any{{"name": "shared", "command": "node", "args": []any{"fixture"}}}
	for _, tc := range []struct {
		name, servers, wantError string
		wantCount                int
	}{
		{"empty", `[]`, "", 1},
		{"distinct-editor-server", `[{"name":"editor","command":"other"}]`, "", 2},
		{"identical-dedup", `[{"args":["fixture"],"command":"node","name":"shared"}]`, "", 1},
		{"conflicting-name", `[{"name":"shared","command":"other"}]`, "conflicting", 0},
		{"missing-name", `[{"command":"other"}]`, "no name", 0},
		{"invalid-shape", `{}`, "must be an array", 0},
	} {
		for _, method := range []string{"session/new", "session/load", "session/resume"} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				child := &Child{MCPServers: func() ([]map[string]any, error) { return shared, nil }}
				original := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":"setup","method":%q,"params":{"cwd":"/work","sessionId":"S1","mcpServers":%s,"extension":{"keep":true}}}`+"\n", method, tc.servers))
				before := clone(original)
				line, err := projectSessionMCP(child, original)
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("projection error = %v, want %s", err, tc.wantError)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(original, before) || !bytes.HasSuffix(line, []byte("\n")) {
					t.Fatal("projection mutated editor input or lost newline framing")
				}
				var params struct {
					Cwd, SessionID string
					MCPServers     []map[string]any
					Extension      map[string]bool
				}
				if err := json.Unmarshal(parse(line).Params, &params); err != nil {
					t.Fatal(err)
				}
				if len(params.MCPServers) != tc.wantCount || params.Cwd != "/work" || params.SessionID != "S1" || !params.Extension["keep"] {
					t.Fatalf("projection lost or replaced parameters: %+v", params)
				}
			})
		}
	}
	failed := &Child{MCPServers: func() ([]map[string]any, error) { return nil, errors.New("foreign handoff") }}
	if _, err := projectSessionMCP(failed, []byte(`{"id":1,"method":"session/new","params":{}}`)); err == nil {
		t.Fatal("handoff failure silently became a tool-free session")
	} else if line := sessionMCPErrorResponse(json.RawMessage(`1`), err); !bytes.Contains(line, []byte(`"code":-32603`)) || !bytes.Contains(line, []byte("reconnect")) {
		t.Fatalf("handoff error lacks server classification or recovery: %s", line)
	}
	for _, method := range []string{"initialize", "session/prompt", "session/set_config_option"} {
		line := []byte(fmt.Sprintf(`{"id":1,"method":%q,"params":{}}`, method))
		if got, err := projectSessionMCP(failed, line); err != nil || !bytes.Equal(got, line) {
			t.Fatalf("unrelated %s consulted or changed shared tools: %s, %v", method, got, err)
		}
	}
}

func TestProxySessionMCPStoresOnlyEditorParams(t *testing.T) {
	input := &recordingWriteCloser{}
	var output bytes.Buffer
	child := &Child{In: input, Provider: "claude", MCPServers: func() ([]map[string]any, error) {
		return []map[string]any{{"name": "shared", "command": "node", "args": []any{"child-standin"}}}, nil
	}}
	p := &proxy{child: child, out: &output, pending: map[string]bool{}, sessions: map[string]*sess{}}
	original := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/work","mcpServers":[]}}` + "\n")
	p.forwardClient(original, originEditor)
	if !strings.Contains(input.String(), "child-standin") || bytes.Contains(p.sessionReqs["1"].params, []byte("child-standin")) {
		t.Fatal("shared projection absent from wire or leaked into stored editor params")
	}
	before := input.Len()
	p.forwardClient([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"mcpServers":[{"name":"shared","command":"other"}]}}`+"\n"), originEditor)
	if input.Len() != before || p.pending["2"] || parse(output.Bytes()).Error == nil {
		t.Fatal("conflicting setup reached child, became pending, or failed without a response")
	}
	if !bytes.Contains(output.Bytes(), []byte(`"code":-32602`)) || !bytes.Contains(output.Bytes(), []byte("unique names in editor and Coop shared configuration")) {
		t.Fatalf("conflict lacks parameter classification or remedy: %s", output.Bytes())
	}
}

func TestProxyReplayMCPUsesReplacementProjection(t *testing.T) {
	first, second := newFakeChild(), newFakeChild()
	first.provider, second.provider = "claude", "claude"
	c1, c2 := first.child(), second.child()
	for marker, child := range map[string]*Child{"old-standin": c1, "new-standin": c2} {
		child.MCPServers = func() ([]map[string]any, error) {
			return []map[string]any{{"name": "shared", "command": "node", "args": []any{marker}}}, nil
		}
	}
	queue := make(chan *Child, 2)
	queue <- c1
	queue <- c2
	clientR, clientW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), clientR, outW, func(context.Context) (*Child, error) { return <-queue, nil }, nil)
	}()
	h := &proxyHarness{clientIn: clientW, clientOut: bufio.NewReader(outR), children: []*fakeChild{first, second}, childIn: []*bufio.Reader{bufio.NewReader(first.inR), bufio.NewReader(second.inR)}, done: done, t: t}
	h.initialize(0)
	writeLine(t, clientW, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work","mcpServers":[{"name":"editor","command":"other"}]}}`)
	if line := readLine(t, h.childIn[0]); !bytes.Contains(line, []byte("old-standin")) {
		t.Fatalf("initial projection absent: %s", line)
	}
	writeLine(t, first.outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"S1"}}`)
	readLine(t, h.clientOut)
	writeLine(t, clientW, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1"}}`)
	readLine(t, h.childIn[0])
	writeLine(t, first.outW, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	readLine(t, h.clientOut)
	first.outW.Close()
	for _, method := range []string{"initialize", "session/load", "session/new"} {
		line := readLine(t, h.childIn[1])
		fr := parse(line)
		if fr.Method != method {
			t.Fatalf("replay method = %s, want %s", fr.Method, method)
		}
		if method != "initialize" && (bytes.Contains(line, []byte("old-standin")) || !bytes.Contains(line, []byte("new-standin")) || !bytes.Contains(line, []byte(`"editor"`))) {
			t.Fatalf("replay kept old tools or lost editor/replacement tools: %s", line)
		}
		if method == "session/load" {
			writeLine(t, second.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32002,"message":"recreate fixture"}}`, fr.ID))
		} else if method == "initialize" {
			writeLine(t, second.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, fr.ID))
		} else {
			writeLine(t, second.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"S2","configOptions":[]}}`, fr.ID))
		}
	}
	readLine(t, h.clientOut)
	if err := h.shutdown(); err != nil {
		t.Fatal(err)
	}
}

func TestProxyReplayMCPTimeoutReleasesStalledWriter(t *testing.T) {
	startup, idle := replayStartupGrace, replayIdleTimeout
	replayStartupGrace, replayIdleTimeout = 150*time.Millisecond, 150*time.Millisecond
	defer func() { replayStartupGrace, replayIdleTimeout = startup, idle }()
	for _, phase := range []string{"load", "recreate"} {
		t.Run(phase, func(t *testing.T) {
			fake := newFakeChild()
			fake.provider = "claude"
			child := fake.child()
			defer child.Stop()
			child.MCPServers = func() ([]map[string]any, error) {
				return []map[string]any{{"name": "shared", "command": "node", "args": []any{strings.Repeat("x", 512<<10)}}}, nil
			}
			p := &proxy{
				out: io.Discard, initialize: []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"),
				sessions: map[string]*sess{"S1": {adapterID: "S1", provider: "claude", turned: true, params: json.RawMessage(`{"cwd":"/work","mcpServers":[]}`)}},
			}
			go func() {
				input := bufio.NewReader(fake.inR)
				line, err := input.ReadBytes('\n')
				if err != nil {
					return
				}
				fmt.Fprintf(fake.outW, `{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", parse(line).ID)
				if phase == "recreate" {
					line, err = input.ReadBytes('\n')
					if err == nil {
						fmt.Fprintf(fake.outW, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32002,"message":"recreate fixture"}}`+"\n", parse(line).ID)
					}
				}
				// The provider remains live but reads no more input. Its next setup write blocks.
			}()
			done := make(chan error, 1)
			go func() { done <- p.replay(child, bufio.NewReader(child.Out)) }()
			select {
			case err := <-done:
				if !errors.Is(err, errReplayTimeout) {
					t.Fatalf("stalled %s replay = %v, want bounded timeout", phase, err)
				}
			case <-time.After(2 * time.Second):
				child.Stop()
				<-done
				t.Fatal("replay timeout left a setup writer blocked")
			}
		})
	}
}
