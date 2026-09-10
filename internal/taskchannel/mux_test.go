package taskchannel

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// echoHandler is a stand-in session: it answers every line with {"echo":<line>,"session":<n>} so a
// test can tell which session a reply came from, and ends when the client hangs up.
func echoHandler() SessionHandler {
	var sessions atomic.Int32
	return func(ctx context.Context, rw io.ReadWriter) error {
		n := sessions.Add(1)
		reader := bufio.NewReader(rw)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				reply, _ := json.Marshal(map[string]any{"echo": strings.TrimSpace(string(line)), "session": n})
				if _, werr := rw.Write(append(reply, '\n')); werr != nil {
					return werr
				}
			}
			if err != nil {
				return nil
			}
		}
	}
}

// fakeHelper speaks the helper protocol from the test side: what the test writes to fromHelper is
// what mux.js would emit; what ServeMux writes to toHelper is what mux.js would receive.
type fakeHelper struct {
	t          *testing.T
	fromHelper *io.PipeWriter
	toHelper   *bufio.Reader
	done       chan error
	ready      chan struct{}
}

func startFakeHelper(t *testing.T, handle SessionHandler) *fakeHelper {
	t.Helper()
	fromR, fromW := io.Pipe()
	toR, toW := io.Pipe()
	h := &fakeHelper{t: t, fromHelper: fromW, toHelper: bufio.NewReaderSize(toR, 8<<20), done: make(chan error, 1), ready: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { h.done <- ServeMux(ctx, fromR, toW, func() { close(h.ready) }, handle) }()
	t.Cleanup(func() {
		cancel()
		fromW.Close()
		toR.Close()
	})
	return h
}

func (h *fakeHelper) emit(frame string) {
	h.t.Helper()
	if _, err := io.WriteString(h.fromHelper, frame+"\n"); err != nil {
		h.t.Fatalf("emit: %v", err)
	}
}

func (h *fakeHelper) next() muxFrame {
	h.t.Helper()
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := h.toHelper.ReadBytes('\n')
		ch <- result{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			h.t.Fatalf("read from mux: %v", r.err)
		}
		var frame muxFrame
		if err := json.Unmarshal(r.line, &frame); err != nil {
			h.t.Fatalf("decode %q: %v", r.line, err)
		}
		return frame
	case <-time.After(10 * time.Second):
		h.t.Fatal("no frame from the mux within 10s")
	}
	return muxFrame{}
}

func TestMuxRoutesInterleavedConnectionsToSeparateSessions(t *testing.T) {
	h := startFakeHelper(t, echoHandler())
	h.emit(`{"e":"ready"}`)
	select {
	case <-h.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("ready never signaled")
	}
	h.emit(`{"c":1,"e":"open"}`)
	h.emit(`{"c":2,"e":"open"}`)
	h.emit(`{"c":2,"d":"two"}`)
	h.emit(`{"c":1,"d":"one"}`)
	got := map[int]string{}
	for i := 0; i < 2; i++ {
		frame := h.next()
		if frame.Data == nil {
			t.Fatalf("unexpected frame %+v", frame)
		}
		got[frame.Conn] = *frame.Data
	}
	if !strings.Contains(got[2], `"echo":"two"`) || !strings.Contains(got[1], `"echo":"one"`) {
		t.Fatalf("replies crossed sessions: %v", got)
	}
	if strings.Contains(got[1], `"session":1`) == strings.Contains(got[2], `"session":1`) {
		t.Fatalf("both connections were served by one session: %v", got)
	}
	// A frame for an unknown connection is dropped, not fatal.
	h.emit(`{"c":9,"d":"nobody"}`)
	// Closing connection 1 ends its session and is acknowledged with a close frame; connection 2
	// keeps working.
	h.emit(`{"c":1,"e":"close"}`)
	if frame := h.next(); frame.Conn != 1 || frame.Event != "close" {
		t.Fatalf("expected close ack for 1, got %+v", frame)
	}
	h.emit(`{"c":2,"d":"again"}`)
	if frame := h.next(); frame.Conn != 2 || frame.Data == nil || !strings.Contains(*frame.Data, `"echo":"again"`) {
		t.Fatalf("connection 2 after 1 closed = %+v", frame)
	}
	// The helper going away ends ServeMux cleanly and closes the remaining session.
	h.fromHelper.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("ServeMux = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeMux did not return after the helper stream ended")
	}
}

func TestMuxRefusesAMalformedHelperFrame(t *testing.T) {
	h := startFakeHelper(t, echoHandler())
	h.emit(`{"c":1,"e":"open"}`)
	h.emit(`not a frame`)
	select {
	case err := <-h.done:
		if err == nil || !strings.Contains(err.Error(), "malformed helper frame") {
			t.Fatalf("ServeMux = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeMux did not fail on a malformed frame")
	}
}

// The real helper script, driven by the host's node: two clients on one socket, served through
// one stdio stream — the shape the box and its peers produce.
func TestMuxScriptMultiplexesRealSocketClients(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the live box test covers the helper in the container")
	}
	script := filepath.Join(t.TempDir(), "mux.js")
	if err := os.WriteFile(script, []byte(MuxScript), 0o644); err != nil {
		t.Fatal(err)
	}
	// macOS caps a unix socket path at 104 bytes; t.TempDir() is long, so bind in /tmp.
	sockDir, err := os.MkdirTemp("/tmp", "coop-mux-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "mcp.sock")
	cmd := exec.Command(node, script, sock)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- ServeMux(ctx, stdout, stdin, func() { close(ready) }, echoHandler()) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("helper ended before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("helper never reported ready")
	}
	if info, err := os.Stat(sock); err != nil || info.Mode().Perm() != 0o666 {
		t.Fatalf("socket = %v %v, want mode 0666", info, err)
	}
	dial := func() (net.Conn, *bufio.Reader) {
		conn, err := net.DialTimeout("unix", sock, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return conn, bufio.NewReaderSize(conn, 1<<20)
	}
	ask := func(conn net.Conn, r *bufio.Reader, req string) map[string]any {
		t.Helper()
		if _, err := io.WriteString(conn, req+"\n"); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var reply map[string]any
		if err := json.Unmarshal(line, &reply); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		return reply
	}
	c1, r1 := dial()
	c2, r2 := dial()
	// Interleave: both connect first, then talk; the mux must keep them apart.
	first := ask(c1, r1, `{"id":1}`)
	second := ask(c2, r2, `{"id":2}`)
	if first["echo"] != `{"id":1}` || second["echo"] != `{"id":2}` || first["session"] == second["session"] {
		t.Fatalf("replies = %v / %v", first, second)
	}
	// A line with every escape-worthy byte survives the JSON framing both ways.
	if reply := ask(c1, r1, "quote\" backslash\\ tab\t unicode ☃"); reply["echo"] != "quote\" backslash\\ tab\t unicode ☃" {
		t.Fatalf("escaping lost bytes: %v", reply)
	}
	c1.Close()
	if reply := ask(c2, r2, `{"id":3}`); reply["echo"] != `{"id":3}` {
		t.Fatalf("c2 after c1 closed = %v", reply)
	}
	c2.Close()
	// Closing the helper's stdin is coop going away: the helper exits on its own.
	stdin.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeMux = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeMux did not end after the helper's stdin closed")
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("helper exit = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not exit after stdin closed")
	}
}
