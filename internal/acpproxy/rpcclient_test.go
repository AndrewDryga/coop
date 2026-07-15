package acpproxy_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
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
}

func newACPClient(w io.Writer) *acpClient {
	return &acpClient{
		enc:     json.NewEncoder(w),
		pending: map[string]chan map[string]any{},
		wake:    make(chan struct{}, 1),
		closed:  make(chan struct{}),
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
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var m map[string]any
			if json.Unmarshal(line, &m) == nil {
				c.mu.Lock()
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
