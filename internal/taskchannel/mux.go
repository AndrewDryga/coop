// Package taskchannel is the transport of coop's in-box task tools: a multiplexer script that runs
// in a helper container beside the box and carries every client connection on the channel's unix
// socket over the helper's stdio, and the host-side demultiplexer that serves each connection.
// It knows nothing about tasks or MCP — internal/taskmcp supplies the session handler — so the
// sandbox owner (internal/box) can import it without reaching the task lifecycle.
package taskchannel

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	// BoxSocketDir is the box-side mountpoint of the channel volume, and BoxSocketPath the
	// socket inside it that every MCP client connects to with `socat STDIO UNIX-CONNECT:`.
	BoxSocketDir  = "/coop/tasks"
	BoxSocketPath = BoxSocketDir + "/mcp.sock"

	// maxMuxFrameBytes bounds one helper frame. The helper caps a raw client line at 4 MiB and
	// JSON-escapes it, so a legitimate frame can be several times larger; anything beyond this is
	// a broken or hostile helper stream and ends the channel rather than the host's memory.
	maxMuxFrameBytes = 32 << 20
)

// MuxScript is the helper-side multiplexer (mux.js): it listens on the channel socket inside the
// VM and carries every client connection over the helper container's stdio. box.Run writes it
// into the run's artifacts dir and mounts it read-only into the HELPER — never into the box.
//
//go:embed mux.js
var MuxScript string

// SessionHandler serves one client connection over rw until the client hangs up; taskmcp's
// Server.Serve is the one the box uses.
type SessionHandler func(ctx context.Context, rw io.ReadWriter) error

// muxFrame is one line of the helper protocol (see mux.js for the grammar).
type muxFrame struct {
	Conn  int     `json:"c,omitempty"`
	Event string  `json:"e,omitempty"`
	Data  *string `json:"d,omitempty"`
}

// ServeMux demultiplexes the helper stream — frames arriving on from, frames sent on to — into one
// session per client connection, each served by handle. ready is called once when the helper
// reports its socket bound. It returns when from ends (the helper exited), on a protocol
// violation, or when ctx is done; every open session is closed on the way out.
func ServeMux(ctx context.Context, from io.Reader, to io.Writer, ready func(), handle SessionHandler) error {
	m := &mux{handle: handle, to: to, sessions: map[int]*muxSession{}, ready: ready}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer m.closeAll()
	reader := bufio.NewReaderSize(from, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := readMuxFrame(reader)
		if len(line) > 0 {
			var frame muxFrame
			if jsonErr := json.Unmarshal(line, &frame); jsonErr != nil {
				return fmt.Errorf("task channel: malformed helper frame: %w", jsonErr)
			}
			m.dispatch(ctx, frame)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func readMuxFrame(reader *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		frame = append(frame, chunk...)
		if len(frame) > maxMuxFrameBytes {
			return nil, errors.New("task channel: helper frame exceeds its bound")
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(frame) > 0 && frame[len(frame)-1] == '\n' {
			frame = frame[:len(frame)-1]
		}
		return frame, err
	}
}

type mux struct {
	handle    SessionHandler
	to        io.Writer
	ready     func()
	readyOnce sync.Once
	writeMu   sync.Mutex
	mu        sync.Mutex
	sessions  map[int]*muxSession
	closing   bool
	wg        sync.WaitGroup
}

// muxSession is one client connection: the server reads what the helper relays through in, and
// writes replies back as frames tagged with the connection id.
type muxSession struct {
	id  int
	in  *io.PipeWriter
	out *io.PipeReader
}

func (m *mux) send(frame muxFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	_, err = m.to.Write(append(data, '\n'))
	return err
}

func (m *mux) dispatch(ctx context.Context, frame muxFrame) {
	switch {
	case frame.Event == "ready" && frame.Conn == 0:
		if m.ready != nil {
			m.readyOnce.Do(m.ready)
		}
	case frame.Event == "open":
		m.open(ctx, frame.Conn)
	case frame.Event == "close":
		m.close(frame.Conn)
	case frame.Data != nil:
		m.mu.Lock()
		session := m.sessions[frame.Conn]
		m.mu.Unlock()
		if session != nil {
			// A slow session applies backpressure to the whole helper stream by design: the
			// pipe is unbuffered, and a client that never reads replies should not make the host
			// buffer its requests without bound.
			_, _ = io.WriteString(session.in, *frame.Data+"\n")
		}
	}
}

func (m *mux) open(ctx context.Context, id int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[id]; exists || id <= 0 {
		return
	}
	out, in := io.Pipe()
	session := &muxSession{id: id, in: in, out: out}
	m.sessions[id] = session
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		rw := struct {
			io.Reader
			io.Writer
		}{out, &sessionWriter{mux: m, id: id}}
		_ = m.handle(ctx, rw)
		// The server is done with this connection: tell the helper so the client sees EOF instead
		// of a hang — unless the whole channel is going down, when the helper is gone or about to
		// be and a write it will never read must not hold the teardown.
		m.mu.Lock()
		closing := m.closing
		m.mu.Unlock()
		if !closing {
			_ = m.send(muxFrame{Conn: id, Event: "close"})
		}
		m.close(id)
	}()
}

func (m *mux) close(id int) {
	m.mu.Lock()
	session := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if session != nil {
		_ = session.in.Close()
	}
}

func (m *mux) closeAll() {
	m.mu.Lock()
	m.closing = true
	ids := make([]int, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.close(id)
	}
	m.wg.Wait()
}

// sessionWriter tags a server reply with its connection and hands it to the helper. The server
// writes exactly one newline-terminated frame per call.
type sessionWriter struct {
	mux *mux
	id  int
}

func (w *sessionWriter) Write(p []byte) (int, error) {
	line := string(p)
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if err := w.mux.send(muxFrame{Conn: w.id, Data: &line}); err != nil {
		return 0, err
	}
	return len(p), nil
}
