//go:build acpe2e

// Real-adapter end-to-end for `coop acp --supervise`: drive the installed coop as an
// ACP client over stdio, docker-kill the supervised box mid-session, and confirm the
// session resumes (still authenticated) with no orphaned container. Needs Docker, a
// built box, and a signed-in claude. Run with `make acp-e2e` (which installs first).
//
// It only ever kills the box THIS test started (diffed by container id), so it is safe
// to run alongside live editor sessions.
package acpproxy_test

import (
	"bufio"
	"context"
	"encoding/json"
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
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if e, ok := m["error"]; ok {
			b, _ := json.Marshal(e)
			return nil, &rpcErr{string(b)}
		}
		return m, nil
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
			return
		}
	}
}

type rpcErr struct{ s string }

func (e *rpcErr) Error() string { return e.s }

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
	before := supervisedBoxIDs(t)

	cmd := exec.Command("coop", "acp", "claude", "--supervise")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	c := &acpClient{enc: json.NewEncoder(stdin), pending: map[int]chan map[string]any{}}
	go c.read(stdout)

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
	mine := diff(supervisedBoxIDs(t), before)
	if len(mine) == 0 {
		t.Fatal("no supervised box appeared for this session")
	}
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
	_ = stdin.Close()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if len(diff(supervisedBoxIDs(t), before)) == 0 {
			return
		}
		time.Sleep(time.Second)
	}
	leaked := diff(supervisedBoxIDs(t), before)
	_ = exec.Command("docker", append([]string{"kill"}, leaked...)...).Run() // best-effort cleanup
	t.Fatalf("supervised boxes leaked after exit: %v", leaked)
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
