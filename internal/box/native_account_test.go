package box

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
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
	next, err := renewNativeAccount(context.Background(), cfg, ag, "work", time.Now().Add(nativeRenewalLead), time.Now().Add(time.Minute))
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
	renewed, err := renewNativeAccount(ctx, cfg, ag, "work", deadline, deadline)
	if err != nil || !called {
		t.Fatal("recoverable account was not renewed", err)
	}
	state, err := native.Inspect(renewed.Artifacts, time.Now())
	if err != nil || state.AccessToken != "inert-recovered-access" {
		t.Fatal("missing recovered access", err)
	}
}

// An access the account cannot renew stays in use until it no longer covers the deadline; coming within
// a running box's renewal lead does not end it early.
func TestNativeRenewalKeepsAnAccessItCannotRenewUntilTheDeadline(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ag, _ := agents.Get("gemini")
	signInInertGemini(t, cfg, ag, "inert-access-only", 5*time.Minute, false)
	now := time.Now()
	if _, err := renewNativeAccount(t.Context(), cfg, ag, "work", now.Add(nativeRenewalLead), now.Add(nativeRenewalDeadline)); err != nil {
		t.Fatalf("an access with 5 minutes left that cannot be renewed: %v; want it kept in use", err)
	}
	later := now.Add(4 * time.Minute)
	if _, err := renewNativeAccount(t.Context(), cfg, ag, "work", later.Add(nativeRenewalLead), later.Add(nativeRenewalDeadline)); err == nil {
		t.Fatal("an access that cannot be renewed was kept in use after it stopped covering the deadline")
	}
}

// An interrupted first setup leaves only the account folder's lock and the temporary file of a
// publication that never landed. With no older login in place that is no account yet: a launch
// reads it as missing and a sign-in starts it afresh. With an older login still in place it may be
// an interrupted removal, so it refuses rather than bring that login back, and a sign-in still
// settles it. Anything else in the folder keeps refusing.
func TestNativeAccountInterruptedFirstSetup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
		extra  string
	}{{name: "empty"}, {name: "older login", legacy: true}, {name: "unknown file", extra: "renewal.json"}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir()}
			ag, _ := agents.Get("codex")
			root := filepath.Join(cfg.ConfigDir, ag.Name(), "credentials", "work")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{".authority.lock", ".authority-" + strings.Repeat("a", 32), tc.extra} {
				if name != "" {
					if err := os.WriteFile(filepath.Join(root, name), []byte("{}"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.legacy {
				legacy := cfg.AgentProfileDir(ag.Name(), "work")
				if err := os.MkdirAll(legacy, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(legacy, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"inert-old-key"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			record, exists, err := readNativeAccount(t.Context(), cfg, ag, "work")
			switch {
			case tc.extra != "":
				if err == nil || !exists || errors.Is(err, errAccountInterrupted) {
					t.Fatalf("a folder holding %s read as %v, exists %v", tc.extra, err, exists)
				}
				return
			case tc.legacy:
				if !errors.Is(err, errAccountInterrupted) || !exists || record != nil {
					t.Fatalf("interrupted setup beside an older login = %v, exists %v", err, exists)
				}
			default:
				if err != nil || exists || record != nil {
					t.Fatalf("interrupted first setup = %+v, exists %v, %v; want no account yet", record, exists, err)
				}
			}
			stage := t.TempDir()
			if err := os.Chmod(stage, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stage, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"inert-fresh-key"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			// The sign-in fences the older login against containers; this runtime has none.
			if err := ImportNativeSignIn(t.Context(), cfg, ag.Name(), "work", stage, emptyContainerRuntime(t)); err != nil {
				t.Fatalf("sign-in after an interrupted setup: %v", err)
			}
			record, exists, err = readNativeAccount(t.Context(), cfg, ag, "work")
			if err != nil || !exists || record == nil || !strings.Contains(string(record.Artifacts["auth.json"]), "inert-fresh-key") {
				t.Fatalf("account after sign-in = %+v, exists %v, %v", record, exists, err)
			}
			if _, err := os.Stat(filepath.Join(root, ".authority-"+strings.Repeat("a", 32))); err != nil {
				t.Fatal("the unpublished temporary file was deleted")
			}
			if _, err := os.Stat(filepath.Join(cfg.AgentProfileDir(ag.Name(), "work"), "auth.json")); tc.legacy && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("sign-in left the older login's grant in place")
			}
		})
	}
}

// A real adapter completes an answer a renewal kept before it died: the next ordinary read publishes
// it through the provider's own Complete, without contacting the provider.
func TestNativeAccountCompletesAKeptAnswerThroughTheAdapter(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ag, _ := agents.Get("gemini")
	files := map[string][]byte{
		"settings.json":        []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`),
		"google_accounts.json": []byte(`{"active":"inert@example.invalid","old":[]}`),
		"oauth_creds.json":     []byte(`{"access_token":"inert-old-access","refresh_token":"inert-refresh-grant","expiry_date":1,"token_type":"Bearer"}`),
	}
	record, err := normalizeNativeAccount(ag, files)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := replaceAccountAuthority(t.Context(), cfg, nativeAccountSpec(ag), "work", record); err != nil {
		t.Fatal(err)
	}
	arrived := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	pending := writeKeptAnswer(t, cfg.NativeAuthorityConfig(), nativeAccountSpec(ag), `{"access_token":"inert-kept-access","expires_in":3600}`, arrived)
	current, exists, err := readNativeAccount(t.Context(), cfg, ag, "work")
	if err != nil || !exists || current.Renewal != pending.ID {
		t.Fatalf("read after a kept answer = %+v, %v", current, err)
	}
	state, err := ag.NativeCredentials().Inspect(current.Artifacts, time.Now())
	if err != nil || state.AccessToken != "inert-kept-access" || !state.ExpiresAt.Equal(arrived.Add(time.Hour)) {
		t.Fatalf("completed grant = %+v, %v", state, err)
	}
}

// emptyContainerRuntime is a container runtime that lists no containers at all.
func emptyContainerRuntime(t *testing.T) runtime.Runtime {
	t.Helper()
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
		return dockersock.Info{ID: "empty-fixture", OSType: "linux", Architecture: "amd64", ServerVersion: "29", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
	}))
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: path}
}
