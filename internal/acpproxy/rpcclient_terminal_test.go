package acpproxy_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

type terminalReplyWriter struct {
	client *acpClient
	reply  string
}

func (w *terminalReplyWriter) Write(data []byte) (int, error) {
	w.client.read(strings.NewReader(w.reply))
	return len(data), nil
}

func TestACPQueuedTerminalReplyWinsOverEOF(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		wantCode    int
		wantError   string
	}{
		{"success", `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"final"}}`, 0, ""},
		{"structured refusal", `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"refused"}}`, -32603, ""},
		{"malformed error", `{"jsonrpc":"2.0","id":1,"error":{"code":"wrong","message":"refused"}}`, 0, "malformed JSON-RPC error"},
		{"no reply", "", 0, "ACP stream closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both reply and EOF are ready before req selects, independent of scheduling.
			for range 64 {
				writer := &terminalReplyWriter{reply: tc.reply}
				client := newACPClient(writer)
				writer.client = client
				response, err := client.req(t.Context(), "session/new", nil)
				switch {
				case tc.wantCode != 0:
					var rpc *rpcErr
					if !errors.As(err, &rpc) || rpc.code != tc.wantCode {
						t.Fatalf("terminal structured error was lost: %v", err)
					}
				case tc.wantError != "":
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("terminal refusal = %v, want %s", err, tc.wantError)
					}
				default:
					result, _ := response["result"].(map[string]any)
					if err != nil || result["sessionId"] != "final" {
						t.Fatalf("terminal reply was lost: %v, %v", response, err)
					}
				}
			}
		})
	}
}

type terminalEventContext struct {
	context.Context
	once    sync.Once
	deliver func()
}

func (c *terminalEventContext) Done() <-chan struct{} {
	c.once.Do(c.deliver)
	return nil
}

func TestACPFinalEventIsRescannedBeforeEOF(t *testing.T) {
	const frame = `{"jsonrpc":"2.0","method":"session/update","params":{"text":"last"}}`
	client := newACPClient(io.Discard)
	ctx := &terminalEventContext{Context: context.Background(), deliver: func() {
		// Done is evaluated after await's first scan; leave only EOF ready, not a wake.
		client.read(strings.NewReader(frame))
		select {
		case <-client.wake:
		default:
		}
	}}
	got, after, err := client.event(ctx, 0, "session/update")
	if err != nil || string(got.Raw) != frame || after != 1 {
		t.Fatalf("final event/index lost to EOF: %s, %d, %v", got.Raw, after, err)
	}
	if _, _, err := client.event(t.Context(), after, "missing"); !errors.Is(err, io.EOF) {
		t.Fatalf("absent event must still refuse at EOF: %v", err)
	}
}

func TestACPFullOrCanceledPendingReplyCannotBlockReaderCleanup(t *testing.T) {
	for _, pending := range []bool{true, false} {
		for _, bounded := range []bool{true, false} {
			client := newACPClientWithLimits(io.Discard, acpClientLimits{})
			if bounded {
				client.limits.MaxFrameBytes = 1024
			}
			first := map[string]any{"result": "first"}
			ch := make(chan map[string]any, 1)
			ch <- first
			if pending {
				client.pending["1"] = ch
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				client.read(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":"duplicate"}`))
			}()
			select {
			case <-done:
			case <-time.After(wait.Deadline):
				t.Fatal("full/canceled pending reply stranded reader cleanup")
			}
			wantErr := io.EOF
			if pending {
				wantErr = errACPDuplicateReply
			}
			if !errors.Is(client.streamError(), wantErr) {
				t.Fatalf("wrong terminal refusal: %v", client.streamError())
			}
			if got := <-ch; got["result"] != "first" {
				t.Fatalf("duplicate replaced first queued reply: %v", got)
			}
		}
	}
}
