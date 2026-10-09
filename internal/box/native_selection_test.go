package box

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestNativeCanonicalSelectionIgnoresRetiredProfilesAndHonorsDefaultKey(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{ConfigDir: t.TempDir(), Egress: "open"}
	ag, _ := agents.Get("codex")
	stage := t.TempDir()
	if err := os.Chmod(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"inert-stored-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"default", "named"} {
		if err := ImportNativeSignIn(ctx, cfg, "codex", account, stage); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("OPENAI_API_KEY=inert-environment-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"default", "named"} {
		selected, exists, err := previewNativeAuthority(ctx, cfg, ag, account)
		want := "inert-stored-key"
		if account == "default" {
			want = "inert-environment-key"
		}
		if err != nil || !exists || selected.state.AccessToken != want {
			t.Fatalf("selection %s: %v", account, err)
		}
		bundle, err := NetworkTargetBundle(cfg, agents.Target{Provider: "codex", Accounts: []string{account}}, egress.ClientACP)
		if err != nil || bundle.Backend != "native-broker" || len(bundle.Core) != 0 {
			t.Fatal("canonical preflight fell back to legacy", err)
		}
		stored, _, err := readNativeAccount(ctx, cfg, ag, account)
		if err != nil || !strings.Contains(string(stored.Artifacts["auth.json"]), "inert-stored-key") {
			t.Fatal("environment replaced canonical authority", err)
		}
		input, err := UsageQuotaAuthority(cfg, "codex", account)
		if err != nil || !input.APIKey || input.ProfileDir != "" || input.Current == nil {
			t.Fatal("quota selected obsolete profile", err)
		}
		state, _, err := input.Current(ctx, time.Now().Add(time.Minute))
		if err != nil || state.AccessToken != "inert-stored-key" {
			t.Fatal("quota callback escaped canonical authority", err)
		}
	}
	if err := RemoveNativeAccount(ctx, cfg, "codex", "default"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := previewNativeAuthority(ctx, cfg, ag, "default"); err == nil {
		t.Fatal("stale environment resurrected removed account")
	}
}

func TestNativeEnvironmentOnlyRunsKeepKeysOutOfPublicSeeds(t *testing.T) {
	for _, tc := range []struct{ provider, key string }{{"claude", "ANTHROPIC_API_KEY"}, {"codex", "OPENAI_API_KEY"}, {"gemini", "GEMINI_API_KEY"}} {
		t.Run(tc.provider, func(t *testing.T) {
			ctx := context.Background()
			cfg := &config.Config{ConfigDir: t.TempDir(), Egress: "open"}
			const secret = "inert-environment-secret"
			if err := os.WriteFile(cfg.EnvFile(), []byte(tc.key+"="+secret+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			run, err := planNativeAccounts(ctx, cfg, runtime.Runtime{}, RunSpec{Agent: tc.provider, Homes: true}, false)
			if err != nil || run == nil || len(run.accounts) != 1 || run.accounts[0].ephemeral == nil {
				t.Fatal("env-only authority not brokered", err)
			}
			public, err := json.Marshal(run.accounts[0].seed)
			if err != nil || strings.Contains(string(public), secret) {
				t.Fatal("real key in public seed")
			}
			if err := run.prepare(ctx, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			run.cancel()
			run.workers.Wait()
			defer run.close()
			data, err := os.ReadFile(filepath.Join(run.dir, tc.provider+".json"))
			var snapshot networkgateway.NativeAccessSnapshot
			if err != nil || json.Unmarshal(data, &snapshot) != nil || snapshot.Credential != secret || snapshot.Revoked {
				t.Fatal("guard cannot use frozen env authority", err)
			}
			if err := RemoveNativeAccount(ctx, cfg, tc.provider, "default"); err != nil {
				t.Fatal(err)
			}
			if _, err := run.refresh(ctx, run.accounts[0]); err == nil {
				t.Fatal("env-only run ignored account revocation")
			}
		})
	}
}

func TestNativePreviewAcceptsCommittedRenewalReceipt(t *testing.T) {
	ctx := context.Background()
	cfg, ag, _, _ := cutoverFixture(t)
	if _, _, err := migrateNativeAccount(ctx, cfg, ag, "work", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	renewed, effect, err := renewAccountAuthority(ctx, cfg, nativeAccountSpec(ag), "work", func(*accountAuthority) (bool, error) { return true, nil }, func(current *accountAuthority, retain func([]byte) error) (*accountAuthority, error) {
		if err := retain([]byte(`{"access_token":"inert-rotated"}`)); err != nil {
			return nil, err
		}
		current.Artifacts["auth.json"] = []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"inert-rotated-key"}`)
		return current, nil
	})
	if err != nil || !effect || renewed.Renewal == "" {
		t.Fatal("renewal receipt missing", err)
	}
	pending := filepath.Join(cfg.ConfigDir, ag.Name(), "credentials", "work", accountRenewalName)
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatal("pending renewal retained", err)
	}
	selected, admitted, err := previewNativeAuthority(ctx, cfg, ag, "work")
	if err != nil || !admitted || selected.state.AccessToken != "inert-rotated-key" {
		t.Fatal("successful renewal became recovery refusal", err)
	}
}
