package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMakesCredentialRootAndSharedSecretsPrivate(t *testing.T) {
	clearAgentEnv(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	root := filepath.Join(xdg, "coop", "agents")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"env":      "OPENAI_API_KEY=keep\n",
		"defaults": "codex=default\n",
		"mcp.json": "{}\n",
	} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	assertPerm(t, root, 0o700)
	for name, body := range map[string]string{
		"env":      "OPENAI_API_KEY=keep\n",
		"defaults": "codex=default\n",
		"mcp.json": "{}\n",
	} {
		path := filepath.Join(root, name)
		assertPerm(t, path, 0o600)
		if got, err := os.ReadFile(path); err != nil || string(got) != body {
			t.Fatalf("%s contents changed: %q, %v", name, got, err)
		}
	}
}

// Every command loads configuration first, so Load must not need a writable HOME: `coop version`
// and `coop help` on a read-only or absent home answered with "create private directory ...
// read-only file system" once the root was created eagerly. A missing root is nothing to protect.
func TestLoadLeavesMissingCredentialRootAbsent(t *testing.T) {
	clearAgentEnv(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	root := filepath.Join(xdg, "coop", "agents")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("Load created %s (err=%v); a read-only command must not need a writable HOME", root, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	assertPerm(t, root, 0o700)
}

func TestLoadRefusesUnsafeCredentialRootAndSecret(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
		want  string
	}{
		{
			name: "root symlink",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Symlink(t.TempDir(), root); err != nil {
					t.Fatal(err)
				}
			},
			want: "real directory",
		},
		{
			name: "root regular file",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(root, []byte("not a directory\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "private directory",
		},
		{
			name: "env symlink",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(root, "env")); err != nil {
					t.Fatal(err)
				}
			},
			want: "regular file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAgentEnv(t)
			xdg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", xdg)
			coop := filepath.Join(xdg, "coop")
			if err := os.MkdirAll(coop, 0o700); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(coop, "agents")
			tc.setup(t, root)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want path and %q", err, tc.want)
			}
		})
	}
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
