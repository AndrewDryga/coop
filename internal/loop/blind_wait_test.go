package loop

import (
	"bytes"
	"strings"
	"testing"
)

func TestFixedSleepSecondsOnlyFlagsObviousLongWaits(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    int
	}{
		{"sleep 120", 120},
		{"cd /workspace && sleep 60s", 60},
		{"sleep 10", 0},
		{"echo sleep 120", 0},
		{"wait $pid", 0},
	} {
		if got := fixedSleepSeconds(tc.command); got != tc.want {
			t.Errorf("fixedSleepSeconds(%q) = %d, want %d", tc.command, got, tc.want)
		}
	}
}

func TestClaudeDecoderReportsOneBlindWait(t *testing.T) {
	var out, tail bytes.Buffer
	d := newStreamDecoder(&out, &tail, "claude", "", "/workspace")
	_, _ = d.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"one","name":"Bash","input":{"command":"sleep 120"}}]}}` + "\n"))
	_, _ = d.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"two","name":"Bash","input":{"command":"sleep 180"}}]}}` + "\n"))
	_, _ = d.Write([]byte(`{"type":"result","subtype":"success","num_turns":2,"duration_ms":10}` + "\n"))
	d.flush()
	if strings.Count(out.String(), "fixed sleep 120s") != 1 || strings.Contains(out.String(), "fixed sleep 180s") {
		t.Fatalf("blind-wait warning was not deduplicated:\n%s", out.String())
	}
	if d.last == nil || d.last.BlindWaitSeconds != 120 {
		t.Fatalf("blind wait telemetry = %+v, want 120s", d.last)
	}
}
