package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// A dotfiles setup links ~/.claude to a real directory elsewhere. That home is the person's own,
// so its history is counted, while the open that guards box-writable homes still refuses a link.
func TestUsageFollowsLinkedHostHome(t *testing.T) {
	home, dotfiles := realTempDir(t), realTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	real := filepath.Join(dotfiles, "claude")
	mustWriteSessionHistory(t, filepath.Join(real, "projects", "p", "a.jsonl"), usageCacheFixture("a", 20))
	link := filepath.Join(home, ".claude")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	spec := (claudeAgent{}).Usage()
	homes := spec.HostHomes()
	if !slices.Equal(homes, []string{real}) {
		t.Fatalf("host homes = %v, want the link's target %s", homes, real)
	}
	if history := ReadUsageHistory(context.Background(), homes[0], spec, nil); len(history.Events) != 1 || history.Partial {
		t.Fatalf("linked host home: events=%d partial=%v", len(history.Events), history.Partial)
	}
	// The same directory named by the environment variable is one home, not two.
	t.Setenv("CLAUDE_CONFIG_DIR", link)
	if homes := spec.HostHomes(); !slices.Equal(homes, []string{real}) {
		t.Fatalf("duplicate through the link: %v", homes)
	}
	// A box-writable home is opened as given, and a link there stays refused.
	if history := ReadUsageHistory(context.Background(), link, spec, nil); len(history.Events) != 0 || !history.Partial {
		t.Fatalf("a linked root was read: events=%d partial=%v", len(history.Events), history.Partial)
	}
}

func TestUsageNativeHostHomes(t *testing.T) {
	// Existing homes come back resolved, and macOS temp directories sit behind a link.
	home, custom := realTempDir(t), realTempDir(t)
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
