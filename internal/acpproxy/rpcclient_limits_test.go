package acpproxy_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLimitedACPClientRejectsOversizeFrameBeforeRetention(t *testing.T) {
	client := newACPClientWithLimits(io.Discard, acpClientLimits{
		MaxFrameBytes: 32, MaxTranscriptBytes: 128, MaxFrames: 4,
	})
	client.read(strings.NewReader(`{"jsonrpc":"2.0","method":"` + strings.Repeat("x", 64) + `"}` + "\n"))
	if err := client.streamError(); !errors.Is(err, errACPFrameLimit) {
		t.Fatalf("stream error = %v, want frame limit", err)
	}
	if stats := client.stats(); stats.Frames != 0 || stats.TranscriptBytes != 0 {
		t.Fatalf("oversize frame was retained: %+v", stats)
	}
}

func TestLimitedACPClientRejectsTranscriptAndFrameCountOverflow(t *testing.T) {
	line := `{"jsonrpc":"2.0","method":"event"}` + "\n"
	for _, tt := range []struct {
		name   string
		limits acpClientLimits
		input  string
		want   error
	}{
		{name: "bytes", limits: acpClientLimits{MaxFrameBytes: 128, MaxTranscriptBytes: len(line) + 1, MaxFrames: 4}, input: line + line, want: errACPTranscriptLimit},
		{name: "frames", limits: acpClientLimits{MaxFrameBytes: 128, MaxTranscriptBytes: 1024, MaxFrames: 1}, input: line + line, want: errACPFrameCountLimit},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newACPClientWithLimits(io.Discard, tt.limits)
			client.read(strings.NewReader(tt.input))
			if err := client.streamError(); !errors.Is(err, tt.want) {
				t.Fatalf("stream error = %v, want %v", err, tt.want)
			}
			if stats := client.stats(); stats.Frames != 1 || stats.TranscriptBytes != len(line) {
				t.Fatalf("retained stats = %+v, want one bounded frame", stats)
			}
		})
	}
}

func TestUnlimitedACPClientKeepsExactFixtureWire(t *testing.T) {
	line := `{"jsonrpc":"2.0","method":"event","params":{"canary":"FIXTURE_CANARY"}}` + "\n"
	client := newACPClient(io.Discard)
	client.read(strings.NewReader(line))
	frames := client.transcript()
	if len(frames) != 1 || string(frames[0].Raw) != line {
		t.Fatalf("unlimited fixture transcript = %#v", frames)
	}
	if stats := client.stats(); stats.Frames != 1 || stats.TranscriptBytes != len(line) {
		t.Fatalf("unlimited fixture stats = %+v", stats)
	}
}

func TestLiveACPDiagnosticRedactsRawErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "RPC", err: &rpcErr{code: -32603, raw: "TOKEN_CANARY /private/path PROMPT_CANARY"}, want: "error_class=json_rpc rpc_code=-32603"},
		{name: "deadline", err: context.DeadlineExceeded, want: "error_class=timeout rpc_code=0"},
		{name: "frame", err: errACPFrameLimit, want: "error_class=frame_limit rpc_code=0"},
		{name: "transcript", err: errACPTranscriptLimit, want: "error_class=transcript_limit rpc_code=0"},
		{name: "frame count", err: errACPFrameCountLimit, want: "error_class=frame_count_limit rpc_code=0"},
		{name: "stream", err: io.EOF, want: "error_class=stream rpc_code=0"},
		{name: "process", err: errors.New("TOKEN_CANARY /private/path PROMPT_CANARY"), want: "error_class=process rpc_code=0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := liveACPDiagnostic("prompt", tt.err, true, acpClientStats{Frames: 7, TranscriptBytes: 99})
			for _, want := range []string{"phase=prompt", tt.want, "frames=7", "transcript_bytes=99", "truncated=true"} {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostic %q missing %q", got, want)
				}
			}
			for _, canary := range []string{"TOKEN_CANARY", "/private/path", "PROMPT_CANARY"} {
				if strings.Contains(got, canary) {
					t.Errorf("diagnostic leaked %q: %s", canary, got)
				}
			}
		})
	}
}
