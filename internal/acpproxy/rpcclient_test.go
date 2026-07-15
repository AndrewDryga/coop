package acpproxy_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

var (
	errACPFrameLimit      = errors.New("ACP frame limit exceeded")
	errACPTranscriptLimit = errors.New("ACP transcript limit exceeded")
	errACPFrameCountLimit = errors.New("ACP frame count limit exceeded")
)

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

type wireFrame struct {
	Raw []byte
	Msg map[string]any
}

type acpClient struct {
	wmu sync.Mutex
	enc *json.Encoder

	mu      sync.Mutex
	pending map[string]chan map[string]any
	frames  []wireFrame
	wake    chan struct{}
	nextID  int
	closed  chan struct{}
	readErr error
	limits  acpClientLimits
	bytes   int
	seen    int
}

type acpClientLimits struct {
	MaxFrameBytes      int
	MaxTranscriptBytes int
	MaxFrames          int
}

type acpClientStats struct {
	Frames          int
	TranscriptBytes int
}

func newACPClient(w io.Writer) *acpClient {
	return newACPClientWithLimits(w, acpClientLimits{})
}

func newACPClientWithLimits(w io.Writer, limits acpClientLimits) *acpClient {
	return &acpClient{
		enc:     json.NewEncoder(w),
		pending: map[string]chan map[string]any{},
		wake:    make(chan struct{}, 1),
		closed:  make(chan struct{}),
		limits:  limits,
	}
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
	key := fmt.Sprint(id)
	ch := make(chan map[string]any, 1)
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
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
		return nil, c.streamError()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *acpClient) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

func (c *acpClient) await(ctx context.Context, after int, match func(wireFrame) bool) (wireFrame, int, error) {
	for {
		c.mu.Lock()
		for i := after; i < len(c.frames); i++ {
			if match(c.frames[i]) {
				frame := c.frames[i]
				c.mu.Unlock()
				return frame, i + 1, nil
			}
		}
		c.mu.Unlock()
		select {
		case <-c.wake:
		case <-c.closed:
			return wireFrame{}, after, c.streamError()
		case <-ctx.Done():
			return wireFrame{}, after, ctx.Err()
		}
	}
}

func (c *acpClient) event(ctx context.Context, after int, method string) (wireFrame, int, error) {
	return c.await(ctx, after, func(frame wireFrame) bool {
		got, _ := frame.Msg["method"].(string)
		return got == method && frame.Msg["id"] == nil
	})
}

func (c *acpClient) transcript() []wireFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]wireFrame, len(c.frames))
	copy(out, c.frames)
	return out
}

func (c *acpClient) stats() acpClientStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return acpClientStats{Frames: c.seen, TranscriptBytes: c.bytes}
}

func (c *acpClient) streamError() error {
	c.mu.Lock()
	err := c.readErr
	c.mu.Unlock()
	if err == nil {
		err = io.EOF
	}
	return fmt.Errorf("ACP stream closed: %w", err)
}

// read records every frame before routing it. Agent requests are rejected minimally
// so an adapter cannot stall while the test still sees the exact editor-facing wire.
func (c *acpClient) read(r io.Reader) {
	if c.limits.MaxFrameBytes > 0 || c.limits.MaxTranscriptBytes > 0 || c.limits.MaxFrames > 0 {
		c.readLimited(r)
		return
	}
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			c.recordFrame(line)
		}
		if err != nil {
			c.finishRead(err)
			return
		}
	}
}

func (c *acpClient) readLimited(r io.Reader) {
	frameLimit := c.limits.MaxFrameBytes
	if frameLimit <= 0 {
		frameLimit = 1 << 20
	}
	br := bufio.NewReaderSize(r, frameLimit+1)
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(line) > frameLimit {
			c.finishRead(errACPFrameLimit)
			return
		}
		if len(line) > 0 {
			c.mu.Lock()
			tooManyFrames := c.limits.MaxFrames > 0 && c.seen >= c.limits.MaxFrames
			tooManyBytes := c.limits.MaxTranscriptBytes > 0 && c.bytes+len(line) > c.limits.MaxTranscriptBytes
			if !tooManyFrames && !tooManyBytes {
				c.seen++
				c.bytes += len(line)
			}
			c.mu.Unlock()
			switch {
			case tooManyFrames:
				c.finishRead(errACPFrameCountLimit)
				return
			case tooManyBytes:
				c.finishRead(errACPTranscriptLimit)
				return
			default:
				c.recordFrame(line)
			}
		}
		if err != nil {
			c.finishRead(err)
			return
		}
	}
}

func (c *acpClient) recordFrame(line []byte) {
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return
	}
	c.mu.Lock()
	if c.limits.MaxTranscriptBytes == 0 && c.limits.MaxFrames == 0 && c.limits.MaxFrameBytes == 0 {
		c.seen++
		c.bytes += len(line)
	}
	c.frames = append(c.frames, wireFrame{Raw: append([]byte(nil), line...), Msg: m})
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}

	_, hasMethod := m["method"]
	switch {
	case hasMethod && m["id"] != nil:
		_ = c.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "error": map[string]any{"code": -32601, "message": "no capability"}})
	case !hasMethod && m["id"] != nil:
		key := fmt.Sprint(m["id"])
		c.mu.Lock()
		ch := c.pending[key]
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

func (c *acpClient) finishRead(err error) {
	c.mu.Lock()
	c.readErr = err
	c.mu.Unlock()
	close(c.closed)
}

func liveACPDiagnostic(phase string, err error, stderrTruncated bool, stats acpClientStats) string {
	if !validLiveACPPhase(phase) {
		phase = "harness"
	}
	class := "assertion"
	rpcCode := 0
	truncated := stderrTruncated
	var rpcError *rpcErr
	switch {
	case errors.As(err, &rpcError):
		class, rpcCode = "json_rpc", rpcError.code
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		class = "timeout"
	case errors.Is(err, errACPFrameLimit):
		class, truncated = "frame_limit", true
	case errors.Is(err, errACPTranscriptLimit):
		class, truncated = "transcript_limit", true
	case errors.Is(err, errACPFrameCountLimit):
		class, truncated = "frame_count_limit", true
	case errors.Is(err, io.EOF):
		class = "stream"
	case err != nil:
		class = "process"
	}
	return fmt.Sprintf(
		"phase=%s error_class=%s rpc_code=%d frames=%d transcript_bytes=%d truncated=%t",
		phase, class, rpcCode, stats.Frames, stats.TranscriptBytes, truncated,
	)
}

func validLiveACPPhase(phase string) bool {
	if phase == "" || len(phase) > 32 {
		return false
	}
	for _, char := range phase {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

type rpcErr struct {
	code int
	raw  string
}

func (e *rpcErr) Error() string { return e.raw }
