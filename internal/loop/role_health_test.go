package loop

import (
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/preset"
)

func TestRoleHealthReportsSuccessFailureAndUnused(t *testing.T) {
	p := &preset.Preset{Roles: []preset.Role{
		{Name: "thinker", Mode: preset.ModeConsult},
		{Name: "critic", Mode: preset.ModeConsult},
		{Name: "fast", Mode: preset.ModeDelegate},
		{Name: "native", Mode: preset.ModeNative, Targets: []agents.Target{{Provider: "claude"}}},
	}}
	records := []PeerRecord{
		{Kind: "role_health", Role: "thinker", Target: "claude:first", Outcome: "failed", Attempts: 1},
		{Kind: "role_health", Role: "thinker", Target: "claude:second", Outcome: "success", Attempts: 1},
		{Kind: "role_health", Role: "critic", Target: "codex:bad", Outcome: "failed", Attempts: 2, Cause: "invalid model"},
	}
	out := captureStderr(t, func() { printRoleHealth(p, records) })
	for _, want := range []string{
		"Preset roles",
		"thinker · succeeded on claude:second after 1 failed target",
		"critic · failed on codex:bad after 2 attempts · invalid model",
		"fast · not used",
		"native · usage not reported by the lead provider",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("role report missing %q:\n%s", want, out)
		}
	}
}

func TestRoleHealthRowsDoNotCreateUsage(t *testing.T) {
	rc := costFromRecords(nil, []PeerRecord{{
		Kind: "role_health", Role: "critic", Provider: "codex", Model: "bad", Outcome: "failed",
	}})
	if len(rc.byModel) != 0 || rc.total.inTok != 0 || rc.total.outTok != 0 || rc.total.usd != 0 {
		t.Fatalf("role health became usage: %+v", rc)
	}
}
