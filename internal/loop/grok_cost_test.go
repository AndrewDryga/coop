package loop

import (
	"bytes"
	"strings"
	"testing"
)

func TestGrokCapturedAccountingFormats(t *testing.T) {
	// Current values are copied from actual Grok 1.0.25 fresh/resume captures in
	// the archived peer-usage task's artifacts/grok-native-shape-1.json. The older
	// format is retained by TestGrokStreamDecoder and counts reasoning separately.
	for _, tc := range []struct {
		name, event string
		in, out     int
		cost        float64
	}{
		{"current fresh", `{"type":"end","num_turns":1,"total_cost_usd":0.00422892,"usage":{"input_tokens":6041,"cache_creation_input_tokens":0,"cache_read_input_tokens":256,"output_tokens":38,"reasoning_tokens":32,"total_tokens":6335}}`, 6297, 38, 0.00422892},
		{"current resume", `{"type":"end","num_turns":1,"total_cost_usd":0.00119612,"usage":{"input_tokens":95,"cache_creation_input_tokens":0,"cache_read_input_tokens":6272,"output_tokens":32,"reasoning_tokens":26,"total_tokens":6399}}`, 6367, 32, 0.00119612},
		{"legacy separate reasoning", `{"type":"end","num_turns":1,"usage":{"input_tokens":16016,"cache_read_input_tokens":11264,"output_tokens":125,"reasoning_tokens":62,"total_tokens":27467}}`, 27280, 187, 0},
		{"legacy no total", `{"type":"end","num_turns":1,"usage":{"input_tokens":10,"output_tokens":5,"reasoning_tokens":3}}`, 10, 8, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, tail bytes.Buffer
			d := newGrokStreamDecoder(&out, &tail, "grok", "default", "", "captured")
			writeSplit(t, d, tc.event+"\n")
			d.flush()
			got := d.lastIterResult()
			if got == nil || got.InTok != tc.in || got.OutTok != tc.out || got.CostUSD != tc.cost {
				t.Fatalf("captured tally = %+v, want in %d out %d cost %v", got, tc.in, tc.out, tc.cost)
			}
			if d.streamOutcome() != streamSucceeded || got.Turns != 1 {
				t.Fatalf("accounting changed terminal outcome: %v, %+v", d.streamOutcome(), got)
			}
			rc := costFromRecords([]StageRecord{{Provider: "grok", Model: "captured", CostUSD: got.CostUSD, InTok: got.InTok, OutTok: got.OutTok}}, nil)
			if rc.total.usd != tc.cost || rc.byModel[0].cost.usd != tc.cost || rc.total.outTok != tc.out {
				t.Fatalf("decoded stage cost lost in summary: %+v", rc)
			}
			if !strings.Contains(out.String(), tokenUsage(tc.in, tc.out)) {
				t.Fatalf("human token summary disagrees with decoded usage: %s", out.String())
			}
		})
	}
}

func TestGrokInvalidCostDoesNotDiscardTerminalUsage(t *testing.T) {
	for _, value := range []string{"null", `"invalid"`, "-1", "1e999", "{}"} {
		t.Run(value, func(t *testing.T) {
			var out bytes.Buffer
			d := newGrokStreamDecoder(&out, nil, "grok", "", "", "")
			d.event([]byte(`{"type":"end","total_cost_usd":` + value + `,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))
			got := d.lastIterResult()
			if got == nil || got.CostUSD != 0 || got.InTok != 10 || got.OutTok != 5 || d.streamOutcome() != streamSucceeded {
				t.Fatalf("invalid optional cost changed terminal usage: %+v, %s", got, out.String())
			}
		})
	}
}
