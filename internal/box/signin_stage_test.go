package box

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// A provider that ended without writing the file that holds its login (stopped with Ctrl-C, or
// closed first) left nothing to save, and says so with ErrNoNativeSignIn so the caller can call it a
// cancel. A login file Coop cannot use is a failure, never a cancel, and no account is written
// either way.
func TestImportingASignInThatWroteNoLoginIsRecognizedAndWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		files          map[string]string
		wantNone       bool
	}{
		{"codex wrote only its config and log", "codex", map[string]string{"config.toml": "model = 'x'\n", "log/codex-login.log": "starting device code login flow\n"}, true},
		{"claude wrote its identity file but no credentials", "claude", map[string]string{".claude.json": `{"numStartups":1}`}, true},
		{"grok wrote nothing", "grok", nil, true},
		{"codex wrote a login Coop cannot use", "codex", map[string]string{"auth.json": "{not json"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, stage := &config.Config{ConfigDir: t.TempDir()}, t.TempDir()
			if err := os.Chmod(stage, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, content := range tc.files {
				path := filepath.Join(stage, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := ImportNativeSignIn(t.Context(), cfg, tc.provider, "work", stage)
			if err == nil {
				t.Fatal("a sign-in with no usable login was saved")
			}
			if got := errors.Is(err, ErrNoNativeSignIn); got != tc.wantNone {
				t.Errorf("errors.Is(%v, ErrNoNativeSignIn) = %v, want %v", err, got, tc.wantNone)
			}
			if _, statErr := os.Stat(filepath.Join(cfg.ConfigDir, tc.provider, "credentials", "work")); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a failed import left account state behind: %v", statErr)
			}
		})
	}
}

// Removing a sign-in's stage is the one deletion a cancelled sign-in runs on whatever path it ends
// by, over a folder that holds a provider's own files. It removes that stage with everything in it
// and nothing else: not the configuration folder, not another account's data, not another sign-in's
// stage, and not what a link points at.
func TestRemoveNativeSignInStageRemovesOnlyThatStage(t *testing.T) {
	cfg := &config.Config{ConfigDir: filepath.Join(t.TempDir(), "coop")}
	_, stage, err := NativeSignInStage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	other := folderWith(t, filepath.Join(cfg.ConfigDir, ".sign-in-12345"), "auth.json")
	folderWith(t, filepath.Join(other, "workspace"), "scratch")
	account := folderWith(t, filepath.Join(cfg.ConfigDir, "codex", "credentials", "work"), "authority.json")
	outside := folderWith(t, filepath.Join(t.TempDir(), "outside"), "keep.txt")
	elsewhere := folderWith(t, filepath.Join(filepath.Dir(outside), ".sign-in-1"), "keep.txt")
	folderWith(t, filepath.Join(stage, "workspace"), "scratch")
	folderWith(t, filepath.Join(stage, "codex"), "auth.json") // a provider's half-written login
	if err := os.Symlink(outside, filepath.Join(cfg.ConfigDir, ".sign-in-link")); err != nil {
		t.Fatal(err)
	}
	kept := []string{cfg.ConfigDir, other, account, outside, elsewhere, filepath.Join(outside, "keep.txt")}
	assertKept := func(t *testing.T) {
		t.Helper()
		for _, path := range kept {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("removing a stage took %s: %v", path, err)
			}
		}
	}

	if err := RemoveNativeSignInStage(cfg, stage); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stage is still there: %v", err)
	}
	assertKept(t)
	if err := RemoveNativeSignInStage(cfg, stage); err != nil {
		t.Errorf("a stage already gone is not a failure: %v", err)
	}

	for _, tc := range []struct{ name, path string }{
		{"the configuration folder", cfg.ConfigDir},
		{"another account's data", filepath.Join(cfg.ConfigDir, "codex")},
		{"a folder inside a stage", filepath.Join(other, "workspace")},
		{"a name that only starts like one", filepath.Join(cfg.ConfigDir, ".sign-in")},
		{"a folder outside the configuration folder", outside},
		{"a stage-named folder elsewhere", elsewhere},
		{"a link named like a stage", filepath.Join(cfg.ConfigDir, ".sign-in-link")},
		{"a relative path", ".sign-in-12345"},
		{"nothing", ""},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			if err := RemoveNativeSignInStage(cfg, tc.path); err == nil {
				t.Errorf("removed %q", tc.path)
			}
			assertKept(t)
		})
	}
}

// folderWith makes dir with one file in it and returns dir.
func folderWith(t *testing.T, dir, file string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}
