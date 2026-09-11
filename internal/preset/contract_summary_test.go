package preset

import (
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
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
