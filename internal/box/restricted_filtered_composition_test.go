package box

import (
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// Read-only/bare and --egress filtered are two sandbox assemblies that a restricted run currently
// refuses to combine (checkRestrictedSpec). Composing them must NOT mean standing up a second
// gateway: the rule for that work is "do not create a second sandbox system", and the gateway is the
// system. So the composition has to run the restricted FILESYSTEM profile through the EXISTING
// filtered launch, which creates its agent box with runtime.CreateContainer.
//
// That path validates its options against a strict allowlist and refuses anything else outright. If
// the restricted profile contained one option the allowlist did not admit, the composition would be
// impossible without either weakening that allowlist or duplicating the launch — so this test pins
// the property the design depends on, before the design leans on it.
func TestRestrictedFilesystemProfileIsAdmissibleOnTheFilteredCreatePath(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node"}
	for _, mode := range []agents.ExecutionMode{agents.ModeReadOnly, agents.ModeBare} {
		t.Run(string(mode), func(t *testing.T) {
			profile := restrictedFilesystemArgs(cfg, mode)
			if len(profile) == 0 {
				t.Fatal("the restricted profile is empty; it is what makes the mode a sandbox")
			}
			if !runtime.ValidDockerCreateOptionsForTest(profile) {
				t.Errorf("the filtered create path refuses the restricted profile, so the two cannot be composed without weakening one of them:\n%v", profile)
			}
			// The two properties that make it a sandbox at all, spelled out so a future edit to the
			// profile cannot quietly drop one and still pass the allowlist check above.
			joined := strings.Join(profile, " ")
			if !strings.Contains(joined, "--read-only") {
				t.Error("the profile no longer makes the container root read-only")
			}
			if !strings.Contains(joined, "--tmpfs "+cfg.HomeInBox+":") || !strings.Contains(joined, "--tmpfs /tmp:") {
				t.Errorf("the profile no longer owns its scratch: %v", profile)
			}
			if mode == agents.ModeBare && !strings.Contains(joined, "--tmpfs "+BareWorkdir+":") {
				t.Errorf("bare no longer gets an empty owned cwd: %v", profile)
			}
		})
	}
}
