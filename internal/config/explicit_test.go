package config

import "testing"

// Explicit distinguishes "the user set this" (env/conf) from "this is the built-in default" — the
// seam the .agent/project.yaml box: overlay keys off (an explicit setting always beats the file).
func TestExplicit(t *testing.T) {
	t.Setenv("COOP_EGRESS", "open")
	c := Load()
	if !c.Explicit("COOP_EGRESS") {
		t.Error("an env-set key must be explicit")
	}
	if c.Memory == "" && c.Explicit("COOP_MEMORY") {
		t.Error("a defaulted key must not be explicit")
	}
	if (&Config{}).Explicit("COOP_EGRESS") {
		t.Error("a zero Config has nothing explicit")
	}
}
