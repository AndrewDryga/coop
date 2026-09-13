package preset

import (
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/consult"
)

// The delegate is told, ONCE, to end with the summary the lead actually reads — and the lead's own
// review/gate/commit duties stay in this contract rather than being repeated after every run.
func TestDelegateContractAsksForOneSummary(t *testing.T) {
	role := &Role{
		Name:    "fast",
		Mode:    ModeDelegate,
		Targets: []agents.Target{{Provider: "gemini", Model: "gemini-3.5-flash"}},
	}
	contract := RoleContract(role)
	const ask = "Finish by summarizing what you changed, what you checked, and what is still"
	if n := strings.Count(contract, ask); n != 1 {
		t.Errorf("the summary must be requested exactly once, got %d:\n%s", n, contract)
	}
	if !strings.Contains(contract, "YOU review its `git diff`, run the gate") {
		t.Errorf("the lead's duties belong in the contract:\n%s", contract)
	}
}

func TestDelegateContractIsLeadIndependent(t *testing.T) {
	for _, extra := range []string{"", "Keep this appended instruction."} {
		r := &Role{Name: "fast", Mode: ModeDelegate, Targets: []agents.Target{{Provider: "gemini"}}, PromptText: extra}
		for _, lead := range agents.Names() {
			if RoleContract(r) != roleContract(r, lead) {
				t.Errorf("delegate contract changed for %s with appendix %q", lead, extra)
			}
		}
	}
}

func TestLeadReviewEvidenceGuidanceParity(t *testing.T) {
	for _, mode := range []string{ModeConsult, ModeNative, ModeDelegate} {
		for _, lead := range []string{"claude", "codex"} {
			p := &Preset{Name: "review", Roles: []Role{{Name: "reviewer", Mode: mode, Targets: []agents.Target{{Provider: "claude"}}}}}
			for _, contract := range []string{LeadContract(p, lead), consult.ConsultInstruction([]string{"claude"})} {
				for _, want := range []string{"complete reply and material diagnostics", "bounded chunks", "not only its tail", "remaining access limits", "assumptions and unrun checks in final/state/log", "not source-verified approval", "Resolve named source gaps yourself", "partial-review qualification", "completed, evidenced review remains usable"} {
					if strings.Count(contract, want) != 1 {
						t.Errorf("mode=%s lead=%s evidence guidance missing or duplicated: %q", mode, lead, want)
					}
				}
			}
		}
	}
}
