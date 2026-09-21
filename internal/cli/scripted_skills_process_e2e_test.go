//go:build providere2e

package cli

import (
	"os"
	"path/filepath"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// A shared `.agent/skills` is supposed to reach EVERY skills-capable client, each at the path its own
// CLI reads. Until now that was proven only as a mount PLAN — a unit test over the intended mounts —
// which cannot catch the plan being assembled correctly and then not surviving the launch.
//
// This drives the real binary and reads what the runtime was actually asked to mount, per provider.
func TestProviderScriptedSharedSkillsReachEveryCapableClient(t *testing.T) {
	suite := newDirectProcessSuite(t)

	skillDir := filepath.Join(suite.layout.Repo, ".agent", "skills", "review-board")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# review-board\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, provider := range agents.Names() {
		adapter, ok := agents.Get(provider)
		if !ok {
			t.Fatalf("registered provider %q has no adapter", provider)
		}
		t.Run(provider, func(t *testing.T) {
			_, trace := suite.run(t, []string{provider}, processScenario(provider, nil, 0, ""))
			run := oneProcessEvent(t, trace, "runtime", "run")
			if run.Run == nil {
				t.Fatal("the runtime was never asked to run a box")
			}
			// The client reads its skills from its OWN home, so the shared directory has to arrive
			// at that provider's path — not merely somewhere in the box.
			want := "<container>" + boxHomeInTrace + "/." + provider + "/skills"
			mounted := false
			for _, mount := range run.Run.Mounts {
				if mount.Target == want {
					mounted = true
					if mount.ReadOnly {
						t.Errorf("%s skills mounted read-only; a client that installs into its skills dir would break: %#v", provider, mount)
					}
				}
			}
			switch {
			case adapter.SkillsCapable() && !mounted:
				t.Errorf("%s discovers skills, but the shared .agent/skills never reached %s:\n%#v", provider, want, run.Run.Mounts)
			case !adapter.SkillsCapable() && mounted:
				t.Errorf("%s does not discover skills, so mounting them at %s is dead weight", provider, want)
			}
		})
	}
}

// boxHomeInTrace is the box home the process fixtures run under; the trace renders container paths
// with a "<container>" prefix.
const boxHomeInTrace = "/home/node"
