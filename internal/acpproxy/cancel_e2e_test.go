//go:build acpe2e

package acpproxy_test

import (
	"context"
	"testing"
)

// Cancel only after a streamed response proves the prompt reached the adapter.
// The deliberately long answer prevents a normal fast reply from racing cancel;
// successful runs consume only the first chunk, not the requested enumeration.
func cancelLivePrompt(t *testing.T, ctx context.Context, live *liveACP, sessionID string) {
	t.Helper()
	mark := live.client.mark()
	type result struct {
		response map[string]any
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := live.client.promptWithoutLimitWait(ctx, map[string]any{
			"sessionId": sessionID,
			"prompt":    []any{map[string]any{"type": "text", "text": "Without using tools, print the integers from 1 through 2000, one per line. Do not abbreviate the list."}},
		})
		done <- result{response, err}
	}()
	started := make(chan error, 1)
	go func() {
		_, _, err := live.client.await(ctx, mark, func(frame wireFrame) bool {
			return liveAssistantText([]wireFrame{frame}) != ""
		})
		started <- err
	}()
	select {
	case got := <-done:
		live.fail(t, "cancel_before_stream", got.err)
	case err := <-started:
		if err != nil {
			live.fail(t, "cancel_stream", err)
		}
	case <-ctx.Done():
		live.fail(t, "cancel_stream", ctx.Err())
	}
	if err := live.client.send(map[string]any{
		"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sessionID},
	}); err != nil {
		live.fail(t, "cancel_send", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			live.fail(t, "cancel_response", got.err)
		}
		body, _ := got.response["result"].(map[string]any)
		if body["stopReason"] != "cancelled" {
			live.fail(t, "cancel_stop_reason", nil)
		}
	case <-ctx.Done():
		live.fail(t, "cancel_response", ctx.Err())
	}
}
