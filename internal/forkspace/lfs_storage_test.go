package forkspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsolatedLFSStorageQualifiesNativeConfigScopes(t *testing.T) {
	for _, scope := range []string{"local", "global-include", "worktree", "command"} {
		t.Run(scope, func(t *testing.T) {
			repo := committedSetupRepo(t)
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
			switch scope {
			case "local":
				gitIn(t, repo, "config", "lfs.storage", "external-cache")
			case "global-include":
				included := filepath.Join(t.TempDir(), "included")
				if err := os.WriteFile(included, []byte("[lfs]\n storage = external-cache\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				global := filepath.Join(t.TempDir(), "global")
				if err := os.WriteFile(global, []byte("[include]\n path = "+included+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GIT_CONFIG_GLOBAL", global)
			case "worktree":
				gitIn(t, repo, "config", "extensions.worktreeConfig", "true")
				gitIn(t, repo, "config", "--worktree", "lfs.storage", "external-cache")
			case "command":
				t.Setenv("GIT_CONFIG_COUNT", "1")
				t.Setenv("GIT_CONFIG_KEY_0", "lfs.storage")
				t.Setenv("GIT_CONFIG_VALUE_0", "external-cache")
			}
			if err := QualifyDefaultLFSStorage(t.Context(), repo); err == nil || !strings.Contains(err.Error(), "default common-directory LFS cache") {
				t.Fatalf("custom native storage scope %s admitted: %v", scope, err)
			}
		})
	}
}
