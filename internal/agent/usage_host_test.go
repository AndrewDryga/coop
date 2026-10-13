package agent

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestUsageNativeHostHomes(t *testing.T) {
	home, custom := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	for _, tc := range []struct {
		provider, dir, env string
		parent             bool
	}{
		{"claude", ".claude", "CLAUDE_CONFIG_DIR", false},
		{"codex", ".codex", "CODEX_HOME", false},
		{"gemini", ".gemini", "GEMINI_CLI_HOME", true},
		{"grok", ".grok", "GROK_HOME", false},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			ag, _ := Get(tc.provider)
			t.Setenv(tc.env, "")
			def := filepath.Join(home, tc.dir)
			if got := ag.Usage().HostHomes(); !slices.Equal(got, []string{def}) {
				t.Fatalf("defaults: %v", got)
			}
			t.Setenv(tc.env, custom)
			want := custom
			if tc.parent {
				want = filepath.Join(custom, tc.dir)
			}
			if got := ag.Usage().HostHomes(); !slices.Equal(got, []string{def, want}) {
				t.Fatalf("custom: %v", got)
			}
			if tc.parent {
				t.Setenv(tc.env, home)
			} else {
				t.Setenv(tc.env, def)
			}
			if got := ag.Usage().HostHomes(); len(got) != 1 {
				t.Fatalf("duplicate: %v", got)
			}
		})
	}
}
