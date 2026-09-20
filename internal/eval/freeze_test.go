package eval

import "testing"

func TestFrozenConfigFingerprintSeparatesEveryChange(t *testing.T) {
	base := FrozenConfig{
		Kind: ConfigPreset, Label: "frontier",
		Content: []byte("lead: codex\n"), LoopConfig: []byte("work:\n"),
		Build: BuildIdentity{Version: "v9.0.0-391-gabc", Digest: "sha256:1"},
	}
	fp := base.Fingerprint()
	if base.Fingerprint() != fp {
		t.Fatal("fingerprint is not stable for identical input")
	}
	// Each of these is a real change to the system under test and must move the fingerprint.
	variants := map[string]FrozenConfig{
		"edited preset content":  withContent(base, []byte("lead: claude\n")),
		"edited loop recipe":     withLoop(base, []byte("work:\n  command: x\n")),
		"different build digest": withDigest(base, "sha256:2"),
		"dirty build":            withDirty(base, true),
		"different label":        withLabel(base, "other"),
	}
	for name, v := range variants {
		if v.Fingerprint() == fp {
			t.Errorf("%s did not change the configuration fingerprint", name)
		}
	}
	// A bare target with no content is stable and distinct from a preset of the same label.
	target := FrozenConfig{Kind: ConfigTarget, Label: "frontier"}
	if target.Fingerprint() == base.Fingerprint() {
		t.Error("a target and a preset with the same label hash the same")
	}
}

func withContent(c FrozenConfig, b []byte) FrozenConfig { c.Content = b; return c }
func withLoop(c FrozenConfig, b []byte) FrozenConfig    { c.LoopConfig = b; return c }
func withDigest(c FrozenConfig, d string) FrozenConfig  { c.Build.Digest = d; return c }
func withDirty(c FrozenConfig, d bool) FrozenConfig     { c.Build.Dirty = d; return c }
func withLabel(c FrozenConfig, l string) FrozenConfig   { c.Label = l; return c }
