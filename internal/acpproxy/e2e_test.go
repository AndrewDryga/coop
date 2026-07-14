//go:build acpe2e

// Real-adapter end-to-end for `coop acp`: drive the installed coop as an ACP client
// over stdio and verify contracts that unit tests cannot prove. Needs Docker, a built
// box, and signed-in providers. Run with `make acp-e2e` (which installs first).
//
// It only ever kills the box THIS test started (diffed by container id), so it is safe
// to run alongside live editor sessions.
package acpproxy_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

type acpClient struct {
	wmu     sync.Mutex
	enc     *json.Encoder
	mu      sync.Mutex
	pending map[int]chan map[string]any
	nextID  int
	closed  chan struct{}
	readErr error
}

func (c *acpClient) send(obj any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.enc.Encode(obj)
}

func (c *acpClient) req(ctx context.Context, method string, params any) (map[string]any, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan map[string]any, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if e, ok := m["error"]; ok && e != nil {
			b, _ := json.Marshal(e)
			obj, ok := e.(map[string]any)
			codeNumber, codeOK := obj["code"].(float64)
			message, messageOK := obj["message"].(string)
			code := int(codeNumber)
			if !ok || !codeOK || float64(code) != codeNumber || !messageOK || message == "" {
				return nil, fmt.Errorf("malformed JSON-RPC error: %s", b)
			}
			return nil, &rpcErr{code: code, raw: string(b)}
		}
		return m, nil
	case <-c.closed:
		c.mu.Lock()
		err := c.readErr
		c.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return nil, fmt.Errorf("ACP stream closed: %w", err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// read correlates responses by id and refuses agent→client requests minimally so the
// adapter never stalls waiting on us.
func (c *acpClient) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var m map[string]any
			if json.Unmarshal(line, &m) == nil {
				_, isReq := m["method"]
				switch {
				case isReq && m["id"] != nil:
					_ = c.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "error": map[string]any{"code": -32601, "message": "no capability"}})
				case !isReq && m["id"] != nil:
					if idf, ok := m["id"].(float64); ok {
						c.mu.Lock()
						ch := c.pending[int(idf)]
						c.mu.Unlock()
						if ch != nil {
							ch <- m
						}
					}
				}
			}
		}
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			close(c.closed)
			return
		}
	}
}

type rpcErr struct {
	code int
	raw  string
}

func (e *rpcErr) Error() string { return e.raw }

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type liveACP struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	client *acpClient
	stderr *lockedBuffer
	done   chan error
	before map[string]bool
	super  string
}

func startLiveACP(t *testing.T, provider string) *liveACP {
	t.Helper()
	before := supervisedBoxIDs(t)
	cmd := exec.Command("coop", "acp", provider)
	cmd.Env = append(os.Environ(), "COOP_ACP_WARM=0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("%s stdin pipe: %v", provider, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		t.Fatalf("%s stdout pipe: %v", provider, err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatalf("start %s ACP: %v", provider, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	c := &acpClient{enc: json.NewEncoder(stdin), pending: map[int]chan map[string]any{}, closed: make(chan struct{})}
	go c.read(stdout)
	live := &liveACP{cmd: cmd, stdin: stdin, client: c, stderr: stderr, done: done, before: before}
	t.Cleanup(func() { live.stop(t) })
	return live
}

func (a *liveACP) captureSupervisor(t *testing.T) []string {
	t.Helper()
	mine := diff(supervisedBoxIDs(t), a.before)
	if len(mine) != 1 {
		t.Fatalf("ACP session owns %d new supervised boxes, want 1: %v", len(mine), mine)
	}
	out, err := exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "coop.sup"}}`, mine[0]).Output()
	if err != nil {
		t.Fatalf("inspect ACP supervisor label: %v", err)
	}
	a.super = strings.TrimSpace(string(out))
	if a.super == "" {
		t.Fatal("ACP test box has no coop.sup label")
	}
	return mine
}

func (a *liveACP) supervisorBoxes(t *testing.T) []string {
	t.Helper()
	if a.super == "" {
		return nil
	}
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=coop.sup="+a.super).Output()
	if err != nil {
		t.Errorf("list ACP test boxes: %v", err)
		return nil
	}
	return strings.Fields(string(out))
}

func (a *liveACP) removeLeakedBoxes(t *testing.T) {
	t.Helper()
	ids := a.supervisorBoxes(t)
	if len(ids) == 0 {
		return
	}
	args := append([]string{"rm", "-f"}, ids...)
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Errorf("remove leaked ACP test boxes %v: %v (%s)", ids, err, strings.TrimSpace(string(out)))
		return
	}
	t.Errorf("ACP process leaked test boxes after exit; removed %v", ids)
}

func (a *liveACP) stop(t *testing.T) {
	t.Helper()
	_ = a.stdin.Close()
	select {
	case err := <-a.done:
		if err != nil {
			t.Errorf("ACP process exited uncleanly after stdin EOF: %v\nstderr:\n%s", err, a.stderr.String())
		}
		a.removeLeakedBoxes(t)
		return
	case <-time.After(20 * time.Second):
	}
	_ = a.cmd.Process.Signal(os.Interrupt)
	select {
	case err := <-a.done:
		t.Errorf("ACP process needed an interrupt after stdin EOF (exit: %v)\nstderr:\n%s", err, a.stderr.String())
		a.removeLeakedBoxes(t)
		return
	case <-time.After(20 * time.Second):
	}
	_ = a.cmd.Process.Kill()
	<-a.done
	a.removeLeakedBoxes(t)
	t.Errorf("ACP process did not stop after stdin EOF and interrupt; stderr:\n%s", a.stderr.String())
}

func supervisedBoxIDs(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-q", "--filter", "label=coop.supervised=1").Output()
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	ids := map[string]bool{}
	for _, id := range strings.Fields(string(out)) {
		ids[id] = true
	}
	return ids
}

// newSession opens a session with a cwd that exists *inside the box*. coop mounts the
// repo at its real host path, so the test's own dir works; a host temp dir would not.
func newSession(ctx context.Context, c *acpClient, cwd string) error {
	_, err := c.req(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	return err
}

func TestSuperviseResume(t *testing.T) {
	if _, err := exec.LookPath("coop"); err != nil {
		t.Skip("coop not on PATH (run `make install`)")
	}
	live := startLiveACP(t, "claude")
	c := live.client

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cwd, _ := os.Getwd() // inside the mounted repo, so it exists in the box

	// initialize + session/new prove the box is up and authenticated.
	if _, err := c.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": true, "writeTextFile": true}}}); err != nil {
		t.Skipf("initialize failed (box not built / not signed in?): %v", err)
	}
	if err := newSession(ctx, c, cwd); err != nil {
		t.Fatalf("session/new before kill (auth?): %v", err)
	}

	// Kill exactly this test's box.
	mine := live.captureSupervisor(t)
	t.Logf("killing this session's box(es): %v", mine)
	_ = exec.Command("docker", append([]string{"kill"}, mine...)...).Run()

	// The supervisor should respawn + re-auth; session/new must succeed again while the
	// new box spins up.
	resumed := false
	for i := 0; i < 20 && ctx.Err() == nil; i++ {
		if err := newSession(ctx, c, cwd); err == nil {
			resumed = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !resumed {
		t.Fatal("session/new never succeeded after the box was killed (resume/auth broke)")
	}

	// Shut down (stdin EOF triggers the supervisor's teardown) and confirm this test's
	// boxes are gone — no orphans.
	_ = live.stdin.Close()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if len(live.supervisorBoxes(t)) == 0 {
			return
		}
		time.Sleep(time.Second)
	}
	leaked := live.supervisorBoxes(t)
	_ = exec.Command("docker", append([]string{"rm", "-f"}, leaked...)...).Run() // best-effort cleanup
	t.Fatalf("supervised boxes leaked after exit: %v", leaked)
}

func TestForeignSessionLoadRejectsUnknownID(t *testing.T) {
	if _, err := exec.LookPath("coop"); err != nil {
		t.Skip("coop not on PATH (run `make install`)")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	const loadTimeout = 10 * time.Second
	cases := []struct {
		provider string
		code     int
		markers  []string
	}{
		{provider: "claude", code: -32002, markers: []string{"Resource not found"}},
		{provider: "codex", code: -32603, markers: []string{"no rollout found for thread id"}},
		{provider: "gemini", code: -32603, markers: []string{"No previous sessions found for this project"}},
		{provider: "grok", code: -32603, markers: []string{"FS_NOT_FOUND", "Path not found"}},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			foreignID := newForeignSessionID(t)
			live := startLiveACP(t, tc.provider)
			initCtx, cancelInit := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancelInit()
			if _, err := live.client.req(initCtx, "initialize", map[string]any{
				"protocolVersion": 1,
				"clientCapabilities": map[string]any{
					"fs": map[string]any{"readTextFile": true, "writeTextFile": true},
				},
			}); err != nil {
				t.Fatalf("initialize %s ACP: %v\nstderr:\n%s", tc.provider, err, live.stderr.String())
			}
			live.captureSupervisor(t)

			loadCtx, cancelLoad := context.WithTimeout(context.Background(), loadTimeout)
			started := time.Now()
			_, err := live.client.req(loadCtx, "session/load", map[string]any{
				"cwd":        cwd,
				"mcpServers": []any{},
				"sessionId":  foreignID,
			})
			elapsed := time.Since(started)
			cancelLoad()
			var rpcError *rpcErr
			switch {
			case err == nil:
				t.Fatalf("%s loaded foreign session %s successfully; want JSON-RPC error", tc.provider, foreignID)
			case errors.As(err, &rpcError):
				if elapsed >= loadTimeout {
					t.Fatalf("%s rejected the foreign session after %s; want under %s", tc.provider, elapsed, loadTimeout)
				}
				if rpcError.code != tc.code {
					t.Fatalf("%s foreign-session error code = %d, want %d: %s", tc.provider, rpcError.code, tc.code, rpcError)
				}
				for _, marker := range append(tc.markers, foreignMarker(tc.provider, foreignID)...) {
					if !strings.Contains(rpcError.raw, marker) {
						t.Fatalf("%s error does not identify an unknown session (missing %q): %s", tc.provider, marker, rpcError)
					}
				}
				t.Logf("%s rejected foreign session in %s: %s", tc.provider, elapsed.Round(time.Millisecond), rpcError)
			default:
				t.Fatalf("%s did not reject foreign session with a JSON-RPC error after %s: %v\nstderr:\n%s", tc.provider, elapsed.Round(time.Millisecond), err, live.stderr.String())
			}
		})
	}
}

func newForeignSessionID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func foreignMarker(provider, id string) []string {
	if provider == "claude" || provider == "codex" {
		return []string{id}
	}
	return nil
}

func diff(after, before map[string]bool) []string {
	var out []string
	for id := range after {
		if !before[id] {
			out = append(out, id)
		}
	}
	return out
}
