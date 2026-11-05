package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// snapshotTree lists every file under root with its bytes, so a test can prove a refusal changed
// nothing on disk.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			out[path] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		out[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A login that builds before v11 saved in profiles/<account> or host-credentials/<account> is no
// longer converted. Every path that judges the account refuses it by name, the message names the
// command that signs in again and the build that converts it, and the old folders stay as they were.
func TestPreV11LoginIsRefusedByNameAndLeftUntouched(t *testing.T) {
	for _, tc := range []struct{ provider, file, where string }{
		{"claude", ".credentials.json", "profiles"},
		{"codex", "auth.json", "profiles"},
		{"grok", "auth.json", "profiles"},
		{"gemini", "oauth_creds.json", "profiles"},
		{"gemini", "gemini-credentials.json", "profiles"},
		{"gemini", "api-key", "host-credentials"},
	} {
		t.Run(tc.provider+"/"+tc.where+"/"+tc.file, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir()}
			ag, _ := agents.Get(tc.provider)
			dir := filepath.Join(cfg.ConfigDir, tc.provider, tc.where, "work")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte("inert-old-login"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := snapshotTree(t, cfg.ConfigDir)
			want := "coop login " + tc.provider + "@work"
			mustRefuse := func(what string, err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "v12.1.0") {
					t.Errorf("%s = %v, want a refusal naming %q and the converting build", what, err, want)
				}
			}
			_, _, err := ensureNativeAccount(t.Context(), cfg, ag, "work")
			mustRefuse("ensureNativeAccount", err)
			_, _, err = previewNativeAuthority(t.Context(), cfg, ag, "work")
			mustRefuse("previewNativeAuthority", err)
			if ag.HostCredential().Declared() {
				_, _, found, err := LoadHostCredential(cfg, ag, "work")
				mustRefuse("LoadHostCredential", err)
				if found {
					t.Error("an old key was read")
				}
				_, err = SelectedHostCredentialPath(cfg, ag, "work")
				mustRefuse("SelectedHostCredentialPath", err)
			}
			if ProfileCredentialReady(cfg, tc.provider, "work", time.Now()) || ProfileAuthed(cfg, tc.provider, "work") {
				t.Error("an old login counted as a sign-in")
			}
			if _, ok := ProfileTokenMtime(cfg, tc.provider, "work"); ok {
				t.Error("an old login supplied a rotation time")
			}
			if got := ProfileSignIn(cfg, tc.provider, "work", time.Now()); got != agents.SignInAgain {
				t.Errorf("ProfileSignIn = %v, want a new sign-in", got)
			}
			after := snapshotTree(t, cfg.ConfigDir)
			if len(after) != len(before) {
				t.Fatalf("the refusal changed the config tree: %d -> %d entries", len(before), len(after))
			}
			for path, content := range before {
				if after[path] != content {
					t.Errorf("%s changed", path)
				}
			}
			if _, err := os.Stat(filepath.Join(cfg.ConfigDir, tc.provider, "credentials")); !os.IsNotExist(err) {
				t.Errorf("a refusal created canonical custody: %v", err)
			}
		})
	}
}

// An account with no old login and no sign-in is simply not signed in: no refusal, nothing created.
func TestAccountWithoutAnyLoginIsNotRefusedAsOld(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		if _, exists, err := ensureNativeAccount(t.Context(), cfg, ag, "work"); err != nil || exists {
			t.Errorf("%s: ensure = %v, %v; want no account and no refusal", name, exists, err)
		}
	}
}

// A home whose history import by an older build never finished cannot be finished by this build, so
// the launch is refused by name instead of using a half-imported home.
func TestUnfinishedHistoryImportIsRefusedByName(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	imports := filepath.Join(root, "imports")
	for _, dir := range []string{home, imports} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	receipt := filepath.Join(imports, "abc.json")
	if err := os.WriteFile(receipt, []byte(`{"Version":1,"Done":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkNativeHistorySettled(home)
	if err == nil || !strings.Contains(err.Error(), "never finished") || !strings.Contains(err.Error(), "MIGRATING.md") {
		t.Fatalf("err = %v, want a refusal naming the unfinished import", err)
	}
	if data, readErr := os.ReadFile(receipt); readErr != nil || string(data) != `{"Version":1,"Done":false}` {
		t.Fatalf("the receipt changed: %q, %v", data, readErr)
	}
}
