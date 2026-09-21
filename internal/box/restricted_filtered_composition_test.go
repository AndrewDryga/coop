package box

import (
	"fmt"
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

// The second precondition, and the one that would fail silently. The filtered launch hardcodes the
// agent box's user (`--user 1000:1000`, filtered_launch.go), while the restricted profile hardcodes
// the OWNER of every scratch tmpfs (`uid=restrictedBoxUID`). Those two numbers are written in
// different files for different reasons and neither mentions the other.
//
// If they ever diverge, a composed read-only/bare + filtered run still starts — and then the agent
// cannot write its own home or /tmp, which reads as a broken client rather than a mismatched
// sandbox. Cheaper to pin the equality than to debug it once.
func TestRestrictedScratchIsOwnedByTheFilteredAgentUser(t *testing.T) {
	const filteredAgentUID = 1000 // filtered_launch.go: options = append(..., "--user", "1000:1000", ...)
	if restrictedBoxUID != filteredAgentUID {
		t.Fatalf("the restricted scratch is owned by uid %d but a filtered agent box runs as uid %d; "+
			"a composed run would have unwritable scratch", restrictedBoxUID, filteredAgentUID)
	}
	profile := restrictedFilesystemArgs(&config.Config{HomeInBox: "/home/node"}, agents.ModeReadOnly)
	want := fmt.Sprintf("uid=%d,gid=%d", filteredAgentUID, filteredAgentUID)
	for i, arg := range profile {
		if arg == "--tmpfs" && i+1 < len(profile) && !strings.Contains(profile[i+1], want) {
			t.Errorf("scratch %q is not owned by the agent user (%s)", profile[i+1], want)
		}
	}
}
