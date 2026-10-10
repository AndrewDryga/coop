package box

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
)

func TestRunSameProviderPeerKeepsLeadAndWiresConsult(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
	seedCanonicalFixture(t, cfg, "claude", "personal")
	cfg.SetActiveProfile("claude", "personal")
	cfg.SetActiveModel("claude", "opus")
	spec := RunSpec{Image: "i", Repo: t.TempDir(), Workdir: "/workspace", Cmd: []string{"claude-agent-acp"},
		Agent: "claude", ConsultLead: "claude", Homes: true, Batch: true, Quiet: true,
		Peers: []agents.Target{{Provider: "claude", Model: "haiku"}}}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	if code, err := Run(cfg, recorderRuntime(t, recorder), spec); err != nil || code != 0 {
		t.Fatalf("Run = (%d, %v)", code, err)
	}
	data, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Fields(string(data))
	for _, want := range []string{"COOP_PRIMARY=claude", "COOP_PEERS=claude", "ANTHROPIC_MODEL=opus", "COOP_PEER_MODEL_CLAUDE=haiku"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %q in launch", want)
		}
	}
	if !strings.Contains(string(data), ":/usr/local/bin/coop-consult:ro") {
		t.Error("explicit same-provider peer has no mounted consult wrapper")
	}
	if got := credentialScope(cfg, spec); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("credential scope = %v, want one provider", got)
	}
	if cfg.ActiveProfile("claude") != "personal" {
		t.Fatal("peer replaced the selected lead account")
	}
}

func TestSameProviderPeerInstructionsAndAuthorization(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	for _, tc := range []struct {
		name string
		spec RunSpec
		want string
	}{
		{"plain", RunSpec{Homes: true, Agent: "claude"}, ""},
		{"explicit primary", RunSpec{Homes: true, Agent: "claude", ConsultLead: "claude", Peers: peerTargets("claude")}, "claude"},
		{"mixed duplicates", RunSpec{Homes: true, Agent: "claude", ConsultLead: "claude", Peers: peerTargets("codex", "claude", "codex", "claude")}, "codex claude"},
		{"homes off", RunSpec{Agent: "claude", ConsultLead: "claude", Peers: peerTargets("claude")}, ""},
		{"login", RunSpec{Homes: true, Agent: "claude", Login: true, Peers: peerTargets("claude")}, ""},
		{"raw", RunSpec{Homes: true, Peers: peerTargets("claude")}, ""},
		{"same provider role", RunSpec{Homes: true, Agent: "claude", Preset: &preset.Preset{Roles: []preset.Role{{Name: "observer", Mode: preset.ModeConsult, Targets: peerTargets("claude")}}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := assembleArgs(cfg, true, tc.spec, nil, "", "", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
			var got string
			for _, arg := range args {
				if strings.HasPrefix(arg, "COOP_PEERS=") {
					got = strings.TrimPrefix(arg, "COOP_PEERS=")
				}
			}
			if got != tc.want {
				t.Errorf("COOP_PEERS = %q, want %q", got, tc.want)
			}
		})
	}
	content, _, wired, _, err := leadInstructionMount(cfg, "claude", nil, []string{"claude"}, "", false)
	if err != nil || !wired || !strings.Contains(content, "coop-consult claude --fresh") {
		t.Fatalf("same-provider directive = (wired %v, err %v): %s", wired, err, content)
	}
}

func TestSameProviderPeerKeepsLeadEffortValidation(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	cfg.SetActiveModel("claude", "opus")
	cfg.SetActiveEffort("claude", "low")
	spec := RunSpec{Homes: true, Agent: "claude", ConsultLead: "claude", Peers: []agents.Target{{Provider: "claude", Model: "opus", Effort: "high"}}}
	if err := checkEfforts(cfg, spec); err != nil {
		t.Fatalf("independent supported efforts refused: %v", err)
	}
	args := modelEnvArgs(cfg, spec, []string{"claude"})
	for _, want := range []string{"CLAUDE_CODE_EFFORT_LEVEL=low", "COOP_PEER_EFFORT_CLAUDE=high"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing independent effort %q in %v", want, args)
		}
	}
	cfg.SetActiveModel("claude", "haiku")
	if err := checkEfforts(cfg, spec); err == nil || !strings.Contains(err.Error(), "claude:haiku/low") {
		t.Fatalf("valid peer masked unsupported lead effort: %v", err)
	}
	cfg.SetActiveModel("claude", "opus")
	spec.Peers = []agents.Target{{Provider: "claude", Model: "haiku", Effort: "high"}}
	if err := checkEfforts(cfg, spec); err == nil || !strings.Contains(err.Error(), "claude:haiku/high") {
		t.Fatalf("unsupported peer effort accepted: %v", err)
	}
}
