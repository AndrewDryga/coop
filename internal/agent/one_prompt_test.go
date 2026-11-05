package agent

import "testing"

// OnePrompt finds each client's one-prompt form, and leaves help, version and sessions alone.
func TestOnePrompt(t *testing.T) {
	for _, tc := range []struct {
		agent string
		cmd   []string
		want  bool
	}{
		{"claude", []string{"claude", "-p", "hi"}, true},
		{"claude", []string{"claude", "--model", "opus", "--print=hi"}, true},
		{"claude", []string{"claude"}, false},
		{"claude", []string{"claude", "--version"}, false},
		{"claude", []string{"claude", "--", "-p"}, false},
		{"gemini", []string{"gemini", "--prompt", "hi"}, true},
		{"grok", []string{"grok", "--single", "hi"}, true},
		{"grok", []string{"grok"}, false},
		{"codex", []string{"codex", "exec", "hi"}, true},
		{"codex", []string{"codex", "--model", "x", "e", "hi"}, true},
		{"codex", []string{"codex", "exec", "--help"}, false},
		{"codex", []string{"codex", "resume"}, false},
		{"codex", []string{"codex"}, false},
	} {
		ag, ok := Get(tc.agent)
		if !ok {
			t.Fatal(tc.agent)
		}
		if got := ag.OnePrompt(tc.cmd); got != tc.want {
			t.Errorf("%s %q = %v, want %v", tc.agent, tc.cmd, got, tc.want)
		}
	}
}
