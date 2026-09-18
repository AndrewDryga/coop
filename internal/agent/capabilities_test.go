package agent

import "testing"

func TestSkillsCapabilities(t *testing.T) {
	want := map[string]bool{"claude": true, "codex": true, "gemini": true, "grok": true}
	for _, name := range Names() {
		ag, _ := Get(name)
		capable, covered := want[name]
		if !covered || ag.SkillsCapable() != capable {
			t.Errorf("%s skills capability = %v, covered=%v", name, ag.SkillsCapable(), covered)
		}
	}
	if Default() != "claude" {
		t.Fatalf("default changed to %q", Default())
	}
}
