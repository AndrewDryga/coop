package liveprovider

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/nativeauth"
)

// These tests intentionally exercise pre-cutover vaults, not today's sign-in writer.
func seedLegacyHostKey(cfg *config.Config, ag agents.Agent, account string, data []byte) error {
	home := cfg.AgentProfileDir(ag.Name(), account)
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	if err := ag.HostCredential().Activate(home); err != nil {
		return err
	}
	dir := filepath.Join(cfg.ConfigDir, ag.Name(), "host-credentials", account)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ag.HostCredential().File), data, 0600)
}

func TestPrepareCanonicalAccountsNeverCopiesRenewalAuthority(t *testing.T) {
	for _, provider := range agents.Names() {
		t.Run(provider, func(t *testing.T) {
			source := &config.Config{ConfigDir: t.TempDir()}
			home := t.TempDir()
			if err := os.Chmod(home, 0700); err != nil {
				t.Fatal(err)
			}
			for name, data := range nativeauth.Files(t, provider, "work") {
				writeSource(t, filepath.Join(home, name), string(data), 0600)
			}
			if err := box.ImportNativeSignIn(t.Context(), source, provider, "work", home); err != nil {
				t.Fatal(err)
			}
			prepared, err := Prepare(source.ConfigDir, filepath.Join(t.TempDir(), "isolated"), []Selection{{Provider: provider, Account: "work"}})
			if err != nil {
				t.Fatal(err)
			}
			if !prepared.CredentialPresent(provider, "work") || !prepared.SafeThrough(provider, "work", time.Now().Add(time.Hour)) {
				t.Fatal("canonical access was not usable")
			}
			copied, found, err := box.SnapshotNativeAccess(t.Context(), &config.Config{ConfigDir: prepared.ConfigDir}, provider, "work")
			if err != nil || !found || !copied.Ready {
				t.Fatalf("isolated canonical account: %v, %v", found, err)
			}
			ag, _ := agents.Get(provider)
			state, err := ag.NativeCredentials().Inspect(copied.Files, time.Now())
			if err != nil || state.Refreshable {
				t.Fatalf("isolated account retains renewal authority: %v", err)
			}
			if err := prepared.VerifySources(); err != nil {
				t.Fatal(err)
			}
			writeSource(t, filepath.Join(source.ConfigDir, provider, "credentials", "work", "renewal.json"), `{"intent":"inert"}`, 0600)
			if err := prepared.VerifySources(); err == nil {
				t.Fatal("renewal intent was not part of source integrity")
			}
		})
	}
}

func TestPrepareCopiesOnlySelectedHostVaultKey(t *testing.T) {
	source := &config.Config{ConfigDir: t.TempDir()}
	ag, _ := agents.Get("gemini")
	for _, account := range []string{"work", "unrelated"} {
		if err := box.SaveHostCredential(source, ag, account, []byte(account+"-KEY_CANARY")); err != nil {
			t.Fatal(err)
		}
	}
	prepared, err := Prepare(source.ConfigDir, filepath.Join(t.TempDir(), "isolated"), []Selection{{Provider: "gemini", Account: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	copyConfig := &config.Config{ConfigDir: prepared.ConfigDir}
	_, value, found, err := box.LoadHostCredential(copyConfig, ag, "work")
	if err != nil || !found || value != "work-KEY_CANARY" {
		t.Fatalf("selected key was not isolated: found=%v, error=%v", found, err)
	}
	if !prepared.CredentialPresent("gemini", "work") || !prepared.SafeThrough("gemini", "work", time.Now().Add(time.Hour)) {
		t.Fatal("isolated host-vault key is not ready for the live test")
	}
	if _, _, found, err := box.LoadHostCredential(copyConfig, ag, "unrelated"); err != nil || found {
		t.Fatalf("unrelated account copied: found=%v, error=%v", found, err)
	}
	rel := filepath.Join("gemini", "credentials", "work", "authority.json")
	assertPrivateCopy(t, filepath.Join(source.ConfigDir, rel), filepath.Join(prepared.ConfigDir, rel))
	if err := filepath.WalkDir(copyConfig.AgentProfileDir("gemini", "work"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if strings.Contains(string(data), "KEY_CANARY") {
			t.Error("host-vault key leaked into the mounted provider home")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := prepared.VerifySources(); err != nil {
		t.Fatal(err)
	}
	writeSource(t, filepath.Join(source.ConfigDir, rel), "changed-KEY_CANARY", 0o600)
	if err := prepared.VerifySources(); err == nil {
		t.Fatal("source host-vault mutation was not detected")
	}
}

func TestPrepareHostVaultMissingAndUnselected(t *testing.T) {
	for _, mode := range []string{"missing", "oauth", "default env"} {
		t.Run(mode, func(t *testing.T) {
			source := &config.Config{ConfigDir: t.TempDir()}
			ag, _ := agents.Get("gemini")
			selection := Selection{Provider: "gemini", Account: "work", SourceDefault: mode == "default env"}
			if err := seedLegacyHostKey(source, ag, "work", []byte("UNUSED_KEY_CANARY")); err != nil {
				t.Fatal(err)
			}
			path, err := box.SelectedHostCredentialPath(source, ag, "work")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Chmod(path, 0o666); err != nil {
					t.Fatal(err)
				}
				if mode == "oauth" {
					writeSource(t, filepath.Join(source.AgentProfileDir("gemini", "work"), "settings.json"), `{"security":{"auth":{"selectedType":"oauth-personal"}}}`, 0o600)
				} else {
					writeSource(t, source.EnvFile(), "GEMINI_API_KEY=SELECTED_ENV_CANARY\n", 0o600)
				}
			}
			prepared, err := Prepare(source.ConfigDir, filepath.Join(t.TempDir(), "isolated"), []Selection{selection})
			if err != nil {
				t.Fatal(err)
			}
			_, _, found, err := box.LoadHostCredential(&config.Config{ConfigDir: prepared.ConfigDir}, ag, "work")
			if err != nil || found {
				t.Fatalf("missing/unselected key copied: found=%v, error=%v", found, err)
			}
			if got := prepared.CredentialPresent("gemini", "work"); got != (mode == "default env") {
				t.Fatalf("configured=%v in %s mode", got, mode)
			}
			if err := prepared.VerifySources(); err != nil {
				t.Fatal(err)
			}
			writeSource(t, path, "NEW_KEY_CANARY", 0o600)
			if err := prepared.VerifySources(); (err != nil) != (mode == "missing") {
				t.Fatalf("vault integrity error in %s mode: %v", mode, err)
			}
		})
	}
}

func TestPrepareRejectsUnsafeHostVaultWithoutDisclosure(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "readable", "writable", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			source := &config.Config{ConfigDir: t.TempDir()}
			ag, _ := agents.Get("gemini")
			if err := seedLegacyHostKey(source, ag, "ACCOUNT_CANARY", []byte("SECRET_CANARY")); err != nil {
				t.Fatal(err)
			}
			path, err := box.SelectedHostCredentialPath(source, ag, "ACCOUNT_CANARY")
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "symlink":
				if err := os.Rename(path, path+".real"); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(path+".real", path)
			case "hardlink":
				err = os.Link(path, path+".link")
			case "readable":
				err = os.Chmod(path, 0o644)
			case "writable":
				err = os.Chmod(path, 0o620)
			case "oversize":
				writeSource(t, path, strings.Repeat("x", 8192), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			_, err = Prepare(source.ConfigDir, filepath.Join(parent, "isolated"), []Selection{{Provider: "gemini", Account: "ACCOUNT_CANARY"}})
			if err == nil {
				t.Fatal("unsafe selected vault was accepted")
			}
			for _, secret := range []string{source.ConfigDir, path, "ACCOUNT_CANARY", "SECRET_CANARY"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("credential error disclosed source information")
				}
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatalf("partial isolated credentials remain: %v, %v", entries, err)
			}
		})
	}
}

func TestHostVaultSelectionChangesSourceInputs(t *testing.T) {
	source := &config.Config{ConfigDir: t.TempDir()}
	ag, _ := agents.Get("gemini")
	selection := []Selection{{Provider: "gemini", Account: "work"}}
	selector := filepath.Join(source.AgentProfileDir("gemini", "work"), "settings.json")
	writeSource(t, selector, `{"security":{"auth":{"selectedType":"oauth-personal"}}}`, 0o600)
	before, err := sourceInputs(source.ConfigDir, selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedLegacyHostKey(source, ag, "work", []byte("KEY_CANARY")); err != nil {
		t.Fatal(err)
	}
	after, err := sourceInputs(source.ConfigDir, selection)
	if err != nil || len(after) != len(before)+1 {
		t.Fatalf("newly selected vault input not distinguished: %d -> %d, %v", len(before), len(after), err)
	}
	if _, err := os.Stat(after[len(after)-1].path); errors.Is(err, os.ErrNotExist) {
		t.Fatal("new input does not name the selected vault file")
	}
}
