package box

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestNativeAccountSignInRenewalReadinessAndRemoval(t *testing.T) {
	cfg, staging := &config.Config{ConfigDir: t.TempDir()}, t.TempDir()
	if err := os.Chmod(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"inert-canonical-key","tokens":{"refresh_token":"stale-other-family"},"projects":{"foreign":"never-import"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ag, _ := agents.Get("codex")
	if err := ImportNativeSignIn(context.Background(), cfg, ag.Name(), "work", staging); err != nil {
		t.Fatal(err)
	}
	first, exists, err := readNativeAccount(context.Background(), cfg, ag, "work")
	if err != nil || !exists || first.Epoch != 1 || first.Revision != 1 {
		t.Fatalf("initial account: %+v, %t, %v", first, exists, err)
	}
	if !ProfileCredentialReady(cfg, ag.Name(), "work", time.Now()) {
		t.Fatal("canonical sign-in is not ready")
	}
	next, err := renewNativeAccount(context.Background(), cfg, ag, "work", time.Now().Add(time.Minute))
	if err != nil || next.Revision != first.Revision {
		t.Fatalf("static key unnecessarily renewed: %+v %v", next, err)
	}
	root := filepath.Join(cfg.ConfigDir, ag.Name(), "credentials", "work")
	if _, err := os.Stat(filepath.Join(root, accountRenewalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("healthy credential wrote renewal intent")
	}
	if err := RemoveNativeAccount(context.Background(), cfg, ag.Name(), "work"); err != nil {
		t.Fatal(err)
	}
	removed, _, err := readNativeAccount(context.Background(), cfg, ag, "work")
	if err != nil || !removed.Revoked || removed.Epoch != 2 {
		t.Fatalf("removal: %+v %v", removed, err)
	}
	// A stale legacy login must not resurrect an explicitly removed account.
	legacy := cfg.AgentProfileDir(ag.Name(), "work")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"stale-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ProfileAuthed(cfg, ag.Name(), "work") || ProfileCredentialReady(cfg, ag.Name(), "work", time.Now()) {
		t.Fatal("revoked canonical state fell back to legacy profile")
	}
	if err := ImportNativeSignIn(context.Background(), cfg, ag.Name(), "work", staging); err != nil {
		t.Fatal(err)
	}
	fresh, _, err := readNativeAccount(context.Background(), cfg, ag, "work")
	if err != nil || fresh.Epoch != 3 || !ProfileCredentialReady(cfg, ag.Name(), "work", time.Now()) {
		t.Fatalf("fresh sign-in did not replace tombstone: %+v %v", fresh, err)
	}
}

func TestNativeAccountUnsafeAuthorityCannotFallback(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ag, _ := agents.Get("codex")
	root := filepath.Join(cfg.ConfigDir, ag.Name(), "credentials", "work")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "authority.json"), []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := cfg.AgentProfileDir(ag.Name(), "work")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"stale-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ProfileAuthed(cfg, ag.Name(), "work") {
		t.Fatal("invalid canonical authority fell back to old profile")
	}
}

func TestNativeHostSignInRecoversUncertainRenewal(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ag, _ := agents.Get("codex")
	stage := t.TempDir()
	if err := os.Chmod(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"inert-fresh-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ImportNativeSignIn(t.Context(), cfg, "codex", "work", stage); err != nil {
		t.Fatal(err)
	}
	_, _, err := renewAccountAuthority(t.Context(), cfg, nativeAccountSpec(ag), "work", nil,
		func(_ *accountAuthority, retain func([]byte) error) (*accountAuthority, error) {
			return nil, errors.Join(retain([]byte("inert uncertain provider response")), errors.New("interrupted"))
		})
	if !errors.Is(err, errAccountRenewalUncertain) {
		t.Fatal("fixture did not retain uncertainty", err)
	}
	if _, _, err := readNativeAccount(t.Context(), cfg, ag, "work"); !errors.Is(err, errAccountRenewalUncertain) {
		t.Fatal("ordinary read accepted uncertain grant", err)
	}
	if err := ImportNativeSignIn(t.Context(), cfg, "codex", "work", stage); err != nil {
		t.Fatal("documented fresh sign-in recovery failed", err)
	}
	current, _, err := readNativeAccount(t.Context(), cfg, ag, "work")
	if err != nil || current.Epoch != 2 {
		t.Fatal("fresh sign-in did not advance authority", err)
	}
	archives, err := filepath.Glob(filepath.Join(cfg.ConfigDir, "codex", "credentials", "work", "recovery-*.json"))
	if err != nil || len(archives) != 1 {
		t.Fatal("uncertain response was not retained", archives, err)
	}
}

type nativeRenewalFixtureAgent struct {
	agents.Agent
	native agents.NativeCredentialSpec
}

func (a nativeRenewalFixtureAgent) NativeCredentials() agents.NativeCredentialSpec { return a.native }

func TestNativeRenewalRecoversMissingAccessDespiteFutureExpiry(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{ConfigDir: t.TempDir()}
	base, _ := agents.Get("gemini")
	deadline := time.Now().Add(time.Minute)
	grant := map[string]any{"refresh_token": "inert-refresh-grant", "expiry_date": deadline.Add(time.Hour).UnixMilli(), "token_type": "Bearer"}
	raw, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"settings.json": []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), "google_accounts.json": []byte(`{"active":"inert@example.invalid","old":[]}`), "oauth_creds.json": raw}
	native := base.NativeCredentials()
	called := false
	native.Renew = func(_ context.Context, input map[string][]byte, _ time.Time, retain func([]byte) error) (map[string][]byte, error) {
		called = true
		if err := retain([]byte(`{"access_token":"inert-recovered-access"}`)); err != nil {
			return nil, err
		}
		grant["access_token"] = "inert-recovered-access"
		data, err := json.Marshal(grant)
		if err != nil {
			return nil, err
		}
		out := map[string][]byte{}
		for name, value := range input {
			out[name] = append([]byte(nil), value...)
		}
		out["oauth_creds.json"] = data
		return out, nil
	}
	ag := nativeRenewalFixtureAgent{Agent: base, native: native}
	record, err := normalizeNativeAccount(ag, files)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := replaceAccountAuthority(ctx, cfg, nativeAccountSpec(ag), "work", record); err != nil {
		t.Fatal(err)
	}
	renewed, err := renewNativeAccount(ctx, cfg, ag, "work", deadline)
	if err != nil || !called {
		t.Fatal("recoverable account was not renewed", err)
	}
	state, err := native.Inspect(renewed.Artifacts, time.Now())
	if err != nil || state.AccessToken != "inert-recovered-access" {
		t.Fatal("missing recovered access", err)
	}
}
