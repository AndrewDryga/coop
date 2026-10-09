//go:build providerlivee2e

package loop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
	"github.com/AndrewDryga/coop/internal/testutil/nativeauth"
)

// Absence means fewer than two configured accounts, not a broken credential or an incompatible
// pair. This live probe selects like-for-like credentials; deterministic tests cover mixed families.
func accountLivePair(cfg *config.Config, provider string) ([]liveprovider.Selection, bool, error) {
	// EffectiveProfiles intentionally treats catalog read errors as absence. Qualification must
	// distinguish an unused provider from a catalog it could not inspect.
	for _, catalog := range []string{filepath.Dir(cfg.AgentProfileDir(provider, "default")), filepath.Join(cfg.ConfigDir, provider, "credentials")} {
		if _, err := os.ReadDir(catalog); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, false, errors.New("cannot read configured account catalog")
		}
	}
	accounts := box.EffectiveProfiles(cfg, provider)
	slices.Sort(accounts)
	if len(accounts) < 2 {
		return nil, false, nil
	}
	selections, err := liveprovider.SelectionsForTargets(cfg, []agents.Target{{Provider: provider, Accounts: accounts}})
	if err != nil {
		return nil, false, errors.New("invalid configured accounts")
	}
	kinds := make([]bool, len(selections))
	brokered := make([]bool, len(selections))
	for i, selection := range selections {
		snapshot, canonical, err := box.SnapshotNativeAccess(context.Background(), cfg, provider, selection.Account)
		if err != nil || canonical && !snapshot.Configured {
			return nil, false, errors.New("cannot inspect configured account family")
		}
		if canonical {
			kinds[i] = snapshot.APIKey
			brokered[i] = true
			continue
		}
		brokered[i], err = liveprovider.BrokersKey(cfg, selection)
		if err != nil {
			return nil, false, errors.New("cannot inspect configured account family")
		}
		kinds[i], err = accountLiveLegacyAPIKey(cfg, selection)
		if err != nil {
			return nil, false, errors.New("cannot inspect legacy account family")
		}
	}
	for i := range selections {
		for j := i + 1; j < len(selections); j++ {
			if kinds[i] == kinds[j] {
				return []liveprovider.Selection{selections[i], selections[j]}, brokered[i] || brokered[j], nil
			}
		}
	}
	return nil, false, errors.New("configured accounts require incompatible network families")
}

const accountLiveInvalidKey = "coop-live-deliberately-invalid-credential"

func accountLiveLegacyAPIKey(cfg *config.Config, selection liveprovider.Selection) (bool, error) {
	ag, _ := agents.Get(selection.Provider)
	profile := cfg.AgentProfileDir(selection.Provider, selection.Account)
	if detector, ok := ag.(agents.StoredAPIKeyDetector); ok {
		key, err := detector.StoredAPIKey(profile)
		if key || err != nil && !errors.Is(err, os.ErrNotExist) {
			return key, err
		}
	}
	path, err := box.SelectedHostCredentialPath(cfg, ag, selection.Account)
	if err != nil {
		return false, err
	}
	if path != "" {
		if _, _, found, err := box.LoadHostCredential(cfg, ag, selection.Account); found || err != nil {
			return found, err
		}
	}
	if selection.SourceDefault {
		marker, _ := ag.AuthMarker()
		_, err := os.Lstat(filepath.Join(profile, marker))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		values := box.EnvFileValues(cfg.EnvFile())
		for _, key := range ag.ActiveCredentialEnvKeys(profile, err == nil) {
			if value := values[key]; value != "" {
				state, err := ag.NativeCredentials().Environment("", key, value)
				return state.APIKey, err
			}
		}
	}
	return false, nil
}

// Fault only a Prepare-owned copy. Closed canaries carry no real access or refresh authority;
// future expiry keeps local readiness checks from skipping the first launch before the service
// can reject it. The second account is never opened for writing.
func faultAccountLiveCopy(cfg *config.Config, selection liveprovider.Selection, brokered bool) error {
	ag, _ := agents.Get(selection.Provider)
	profile := cfg.AgentProfileDir(selection.Provider, selection.Account)
	snapshot, canonical, err := box.SnapshotNativeAccess(context.Background(), cfg, selection.Provider, selection.Account)
	if err != nil || canonical && !snapshot.Ready {
		return errors.New("inspect isolated canonical credential")
	}
	apiKey := brokered
	if !canonical {
		apiKey, err = accountLiveLegacyAPIKey(cfg, selection)
		if err != nil {
			return errors.New("inspect isolated legacy credential family")
		}
	}
	if canonical {
		apiKey = snapshot.APIKey
		profile, err = os.MkdirTemp(cfg.ConfigDir, ".invalid-access-")
		if err != nil {
			return errors.New("stage isolated credential fault")
		}
		defer os.RemoveAll(profile)
		for name, data := range snapshot.Files {
			if err := config.WriteFileAtomic(filepath.Join(profile, name), data); err != nil {
				return errors.New("stage isolated access artifact")
			}
		}
	}
	if selection.SourceDefault {
		values := box.EnvFileValues(cfg.EnvFile())
		changed := false
		for _, key := range ag.CredentialEnvKeys() {
			if _, ok := values[key]; ok {
				values[key], changed = accountLiveInvalidKey, true
			}
		}
		if changed {
			keys := make([]string, 0, len(values))
			for key := range values {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			var lines []string
			for _, key := range keys {
				lines = append(lines, key+"="+values[key])
			}
			if err := config.WriteFileAtomic(cfg.EnvFile(), []byte(strings.Join(lines, "\n")+"\n")); err != nil {
				return errors.New("write isolated invalid environment")
			}
		}
	}
	marker, _ := ag.AuthMarker()
	if canonical && selection.Provider == "gemini" && !apiKey {
		marker = "oauth_creds.json"
	}
	path := filepath.Join(profile, marker)
	if ag.HostCredential().Declared() && apiKey {
		if err := box.SaveHostCredential(cfg, ag, selection.Account, []byte(accountLiveInvalidKey)); err != nil {
			return errors.New("write isolated invalid host key")
		}
	} else if _, err := os.Stat(path); err == nil {
		var value any
		switch selection.Provider {
		case "claude":
			value = map[string]any{"claudeAiOauth": map[string]any{
				"accessToken": accountLiveInvalidKey, "expiresAt": time.Now().Add(24 * time.Hour).UnixMilli(),
				"scopes": []string{"user:inference"},
			}}
		case "codex":
			if apiKey {
				value = map[string]any{"auth_mode": "apikey", "OPENAI_API_KEY": accountLiveInvalidKey}
			} else {
				payload, _ := json.Marshal(map[string]any{
					"sub": "coop-live-invalid", "exp": time.Now().Add(24 * time.Hour).Unix(),
					"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "coop-live-invalid", "chatgpt_plan_type": "plus"},
				})
				jwt := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
					base64.RawURLEncoding.EncodeToString(payload) + ".aW52YWxpZA"
				value = map[string]any{"auth_mode": "chatgpt", "last_refresh": time.Now().UTC().Format(time.RFC3339),
					"tokens": map[string]string{"id_token": jwt, "access_token": jwt, "refresh_token": "", "account_id": "coop-live-invalid"}}
			}
		case "gemini":
			value = map[string]any{"access_token": accountLiveInvalidKey,
				"expiry_date": time.Now().Add(24 * time.Hour).UnixMilli(), "token_type": "Bearer"}
		case "grok":
			// Retain only the public issuer/client routing of every projected entry. Replacing just
			// one would leave a real fallback key; copying an open object could retain new secrets.
			var entries map[string]struct {
				Issuer string `json:"oidc_issuer"`
				Client string `json:"oidc_client_id"`
				Mode   string `json:"auth_mode"`
			}
			data, err := os.ReadFile(path) // bounded, regular private file produced by Prepare
			if err != nil || json.Unmarshal(data, &entries) != nil || len(entries) == 0 {
				return errors.New("decode isolated Grok routes")
			}
			canaries := map[string]any{}
			for key, entry := range entries {
				if entry.Issuer == "" || entry.Client == "" || (entry.Mode != "oidc" && entry.Mode != "oauth" && entry.Mode != "api_key") {
					return errors.New("unsupported isolated Grok route")
				}
				canaries[key] = map[string]string{"key": accountLiveInvalidKey, "auth_mode": entry.Mode,
					"oidc_issuer": entry.Issuer, "oidc_client_id": entry.Client, "principal_id": "coop-live-invalid",
					"principal_type": "user", "user_id": "coop-live-invalid", "team_id": "coop-live-invalid",
					"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), "create_time": "2026-01-01T00:00:00Z"}
			}
			value = canaries
		default:
			return errors.New("unsupported isolated credential fault")
		}
		data, err := json.Marshal(value)
		if err != nil || config.WriteFileAtomic(path, append(data, '\n')) != nil {
			return errors.New("write isolated invalid credential")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect isolated credential")
	}
	if canonical && !(ag.HostCredential().Declared() && apiKey) {
		if err := box.ImportNativeSignIn(context.Background(), cfg, selection.Provider, selection.Account, profile); err != nil {
			return errors.New("publish isolated invalid credential")
		}
	}
	kind, err := liveprovider.BrokersKey(cfg, selection)
	if err != nil || kind != brokered || !box.ProfileCredentialReady(cfg, selection.Provider, selection.Account, time.Now()) {
		return errors.New("invalid credential canary changed local readiness or network family")
	}
	return nil
}

func TestProviderAccountsLiveContractCredentialFault(t *testing.T) {
	for _, provider := range agents.Names() {
		t.Run(provider, func(t *testing.T) {
			source := &config.Config{ConfigDir: t.TempDir()}
			ag, _ := agents.Get(provider)
			for _, account := range []string{"first", "second"} {
				stage := t.TempDir()
				if err := os.Chmod(stage, 0700); err != nil {
					t.Fatal(err)
				}
				for name, data := range nativeauth.Files(t, provider, account) {
					if err := os.WriteFile(filepath.Join(stage, name), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := box.ImportNativeSignIn(t.Context(), source, provider, account, stage); err != nil {
					t.Fatal(err)
				}
			}
			pair, brokered, err := accountLivePair(source, provider)
			if err != nil || len(pair) != 2 {
				t.Fatalf("select pair: %v", err)
			}
			prepared, err := liveprovider.Prepare(source.ConfigDir, filepath.Join(t.TempDir(), "copy"), pair)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = prepared.Revoke() }()
			cfg := &config.Config{ConfigDir: prepared.ConfigDir}
			// Another isolated projection gives us an opaque integrity witness for account two.
			second, err := liveprovider.Prepare(cfg.ConfigDir, filepath.Join(t.TempDir(), "second-witness"), pair[1:])
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = second.Revoke() }()
			original, found, err := box.SnapshotNativeAccess(t.Context(), cfg, provider, pair[0].Account)
			if err != nil || !found || !original.Ready {
				t.Fatal("missing isolated access", err)
			}
			originalState, err := ag.NativeCredentials().Inspect(original.Files, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err := faultAccountLiveCopy(cfg, pair[0], brokered); err != nil {
				t.Fatal(err)
			}
			if err := second.VerifySources(); err != nil {
				t.Fatal("fault touched second account")
			}
			if err := prepared.VerifySources(); err != nil {
				t.Fatal("fault touched source credentials")
			}
			faulted, found, err := box.SnapshotNativeAccess(t.Context(), cfg, provider, pair[0].Account)
			if err != nil || !found || !faulted.Ready {
				t.Fatal("missing faulted authority", err)
			}
			state, err := ag.NativeCredentials().Inspect(faulted.Files, time.Now())
			if err != nil || state.Refreshable || state.AccessToken == originalState.AccessToken {
				t.Fatal("fault retained original credential authority", err)
			}
			if state.Selection != originalState.Selection || state.APIKey != originalState.APIKey {
				t.Fatal("fault changed the selected credential family")
			}
			if provider != "codex" && state.AccessToken != accountLiveInvalidKey {
				t.Fatal("fault did not select the invalid access canary")
			}
		})
	}
}

func TestProviderAccountsLiveContractAvailability(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	pair, _, err := accountLivePair(cfg, "codex")
	if err != nil || len(pair) != 0 {
		t.Fatal("zero accounts should be not_configured")
	}
	for _, account := range []string{"first", "second"} {
		if err := os.MkdirAll(cfg.AgentProfileDir("codex", account), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pair, _, err = accountLivePair(cfg, "codex")
	if err != nil || len(pair) != 2 {
		t.Fatal("two empty configured accounts must reach failing preflight, not not_configured")
	}
	if err := os.WriteFile(filepath.Join(cfg.AgentProfileDir("codex", "first"), "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := accountLivePair(cfg, "codex"); err == nil {
		t.Fatal("incompatible configured pair accepted")
	}
}

func TestProviderAccountsLiveContractMixedLayouts(t *testing.T) {
	for _, apiKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "canonical-and-legacy-oauth", true: "legacy-mixed-families"}[apiKey], func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir()}
			for _, account := range []string{"first", "second"} {
				home := cfg.AgentProfileDir("codex", account)
				if !apiKey && account == "first" {
					home = t.TempDir()
					if err := os.Chmod(home, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.MkdirAll(home, 0700); err != nil {
					t.Fatal(err)
				}
				data := nativeauth.Files(t, "codex", account)["auth.json"]
				if apiKey && account == "first" {
					data = []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"INERT_KEY"}`)
				}
				if err := os.WriteFile(filepath.Join(home, "auth.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				if !apiKey && account == "first" {
					if err := box.ImportNativeSignIn(t.Context(), cfg, "codex", account, home); err != nil {
						t.Fatal(err)
					}
				}
			}
			pair, brokered, err := accountLivePair(cfg, "codex")
			if apiKey {
				if err == nil {
					t.Fatal("legacy mixed families passed like-for-like live selection")
				}
			} else if err != nil || len(pair) != 2 || !brokered {
				t.Fatal("same-family mixed layouts were refused", err)
			}
		})
	}
}

func TestProviderAccountsLiveContractCatalogFailure(t *testing.T) {
	for _, kind := range []string{"regular file", "unreadable directory"} {
		t.Run(kind, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir()}
			catalog := filepath.Dir(cfg.AgentProfileDir("codex", "default"))
			if err := os.MkdirAll(filepath.Dir(catalog), 0o700); err != nil {
				t.Fatal(err)
			}
			if kind == "regular file" {
				if err := os.WriteFile(catalog, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(catalog, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(catalog, 0o700); err != nil {
						t.Error(err)
					}
				})
				if _, err := os.ReadDir(catalog); err == nil {
					t.Skip("directory permissions are not enforced for this user")
				}
			}
			if _, _, err := accountLivePair(cfg, "codex"); err == nil {
				t.Fatal("unreadable account catalog became not_configured")
			}
		})
	}
}

func TestProviderAccountsLiveContractDefaultEnv(t *testing.T) {
	for _, def := range []string{"first", "second"} {
		t.Run(def, func(t *testing.T) {
			source := &config.Config{ConfigDir: t.TempDir()}
			ag, _ := agents.Get("gemini")
			for _, account := range []string{"first", "second"} {
				if err := box.SaveHostCredential(source, ag, account, []byte("SOURCE_HOST_KEY_"+account)); err != nil {
					t.Fatal(err)
				}
			}
			if err := source.SetDefaultProfile("gemini", def); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source.EnvFile(), []byte("GEMINI_API_KEY=SOURCE_ENV_KEY\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			pair, brokered, err := accountLivePair(source, "gemini")
			if err != nil || len(pair) != 2 || !brokered {
				t.Fatalf("select env pair: %v", err)
			}
			prepared, err := liveprovider.Prepare(source.ConfigDir, filepath.Join(t.TempDir(), "copy"), pair)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = prepared.Revoke() }()
			cfg := &config.Config{ConfigDir: prepared.ConfigDir}
			second, err := liveprovider.Prepare(cfg.ConfigDir, filepath.Join(t.TempDir(), "witness"), pair[1:])
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = second.Revoke() }()
			if err := faultAccountLiveCopy(cfg, pair[0], brokered); err != nil {
				t.Fatal(err)
			}
			if err := second.VerifySources(); err != nil {
				t.Fatal("second default env credential changed")
			}
			if err := prepared.VerifySources(); err != nil {
				t.Fatal("source env changed")
			}
			want := "SOURCE_ENV_KEY"
			if def == "first" {
				want = accountLiveInvalidKey
			}
			if box.EnvFileValues(cfg.EnvFile())["GEMINI_API_KEY"] != want {
				t.Fatal("wrong default env credential faulted")
			}
		})
	}
}
