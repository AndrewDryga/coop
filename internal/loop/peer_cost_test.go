package loop

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

func TestPeerReportedCostReachesModelAndRunTotals(t *testing.T) {
	// Decode the on-disk shape: old token-only rows remain readable, while a
	// provider-reported cost contributes without estimating prices from tokens.
	var peers []PeerRecord
	if err := json.Unmarshal([]byte(`[
		{"provider":"claude","model":"test","in":100,"out":10,"cost":0.25},
		{"provider":"claude","model":"test","in":200,"out":20,"cost":0.5},
		{"provider":"codex","model":"test","in":300,"out":30}
	]`), &peers); err != nil {
		t.Fatal(err)
	}
	rc := costFromRecords([]StageRecord{{Provider: "claude", Model: "test", CostUSD: 1,
		InTok: 40, OutTok: 4, Finished: []string{"task"}}}, peers)
	if got := rc.total; got.usd != 1.75 || got.inTok != 640 || got.outTok != 64 {
		t.Fatalf("run total = %+v, want $1.75 and 640/64 tokens", got)
	}
	if got := rc.byTask["task"]; got.usd != 1 || got.inTok != 40 || got.outTok != 4 {
		t.Fatalf("unattributed peer usage changed the task's own stage tally: %+v", got)
	}
	if len(rc.byModel) != 2 {
		t.Fatalf("models = %+v, want lead/peer bucket plus token-only peer", rc.byModel)
	}
	if got := rc.byModel[0]; got.model != "claude:test" || got.cost.usd != 1.75 || got.cost.inTok != 340 || got.cost.outTok != 34 {
		t.Fatalf("combined lead/peer bucket = %+v", got)
	}
	if got := rc.byModel[1]; got.model != "codex:test" || got.cost.usd != 0 || got.cost.inTok != 300 || got.cost.outTok != 30 {
		t.Fatalf("token-only peer was priced or lost: %+v", got)
	}
}

func TestPeerInvalidReportedCostDoesNotPoisonTotals(t *testing.T) {
	for _, cost := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		rc := costFromRecords(nil, []PeerRecord{{Provider: "claude", Model: "test", Cost: cost, In: 2, Out: 1}})
		if rc.total.usd != 0 || rc.byModel[0].cost.usd != 0 || rc.total.inTok != 2 || rc.total.outTok != 1 {
			t.Fatalf("invalid reported cost %v corrupted valid token totals: %+v", cost, rc)
		}
	}
}

func TestPeerCostArchiveAndHumanSummary(t *testing.T) {
	repo := t.TempDir()
	path, err := preparePeerRecordFile(repo, "reported-cost")
	if err != nil {
		t.Fatal(err)
	}
	data := `{"provider":"claude","model":"peer-only","in":12,"out":4,"cost":0.25}
{"provider":"codex","model":"token-only","in":20,"out":5}
{"provider":"claude","model":"malformed","cost":"invalid"}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := costForRepo(repo)
	if rc.total.usd != 0.25 || rc.total.inTok != 32 || rc.total.outTok != 9 || len(rc.byModel) != 2 {
		t.Fatalf("archived peer records = %+v", rc)
	}
	usd, _ := WorkspaceCost(repo)
	if usd != 0.25 {
		t.Fatalf("workspace cost = %v, want reported $0.25", usd)
	}
	output := captureStderr(t, func() { printRunSummary(nil, rc, newLoopHealth()) })
	for _, want := range []string{"claude:peer-only", "$0.25 · 12 in · 4 out", "codex:token-only", "not reported · 20 in · 5 out"} {
		if !strings.Contains(output, want) {
			t.Errorf("human digest missing %q:\n%s", want, output)
		}
	}
}
