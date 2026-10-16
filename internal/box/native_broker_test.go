package box

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestNativeClaudeAPIHomeAdmitsLaterCanonicalSubscription(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), Egress: "open", HomeInBox: "/home/node"}
	if err := os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_API_KEY=inert-first-api-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	home, err := PrepareNativeHome(t.Context(), cfg, runtime.Runtime{}, "claude", "default", repo, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.WithNativeHomes(map[string]string{"claude": home})
	spec := RunSpec{Agent: "claude", Homes: true, Repo: repo}
	first, err := planNativeRun(t.Context(), cfg, runtime.Runtime{}, spec)
	if err != nil || first == nil || first.accounts[0].seed.Family != "apikey" {
		t.Fatal("initial native API home unavailable", err)
	}
	seedCanonicalFixture(t, cfg, "claude", "default")
	next, err := planNativeRun(t.Context(), cfg, runtime.Runtime{}, spec)
	if err != nil || next == nil || next.accounts[0].seed.Family != "claude-oauth" {
		t.Fatal("public API home blocked host subscription", err)
	}
	data := mustReadFile(t, filepath.Join(home, ".credentials.json"))
	if !strings.Contains(string(data), next.accounts[0].seed.Marker) || strings.Contains(string(data), "ACCESS_CANARY") || strings.Contains(string(data), "REFRESH_CANARY") {
		t.Fatal("family transition projected a host grant")
	}
}

func TestNativeClaudePublicFamiliesPreserveLocalSettings(t *testing.T) {
	ag, _ := agents.Get("claude")
	native := ag.NativeCredentials()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	settings := []byte(`{"theme":"dark","customLocalSetting":{"keep":true}}`)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), settings, 0600); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"apikey", "claude-oauth", "apikey", "claude-oauth"} {
		seed, err := native.Broker.Seed(family)
		if err != nil {
			t.Fatal(err)
		}
		if err := prepareNativeAuth(t.Context(), home, native, &seed); err != nil {
			t.Fatalf("family %s: %v", family, err)
		}
		if !bytes.Equal(mustReadFile(t, filepath.Join(home, ".claude.json")), settings) {
			t.Fatal("family switch replaced native settings")
		}
		if family == "claude-oauth" && !bytes.Contains(mustReadFile(t, filepath.Join(home, ".credentials.json")), []byte(seed.Marker)) {
			t.Fatal("subscription selector absent")
		}
	}
}

func TestNativeClaudeFamilyTransitionRefusesLocalRealGrants(t *testing.T) {
	ag, _ := agents.Get("claude")
	native := ag.NativeCredentials()
	for _, previousFamily := range []string{"apikey", "claude-oauth"} {
		t.Run(previousFamily, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			previous, err := native.Broker.Seed(previousFamily)
			if err != nil {
				t.Fatal(err)
			}
			if err := prepareNativeAuth(t.Context(), home, native, &previous); err != nil {
				t.Fatal(err)
			}
			receipt := filepath.Join(filepath.Dir(home), "auth", "seed.json")
			before := mustReadFile(t, receipt)
			changed := []byte(`{"claudeAiOauth":{"accessToken":"inert-local-real-access","refreshToken":"inert-local-real-refresh"}}`)
			path := filepath.Join(home, ".credentials.json")
			if err := os.WriteFile(path, changed, 0600); err != nil {
				t.Fatal(err)
			}
			nextFamily := "claude-oauth"
			if previousFamily == "claude-oauth" {
				nextFamily = "apikey"
			}
			next, err := native.Broker.Seed(nextFamily)
			if err != nil {
				t.Fatal(err)
			}
			if err := prepareNativeAuth(t.Context(), home, native, &next); err == nil {
				t.Fatal("local grant blessed by transition")
			}
			if !bytes.Equal(mustReadFile(t, path), changed) || !bytes.Equal(mustReadFile(t, receipt), before) {
				t.Fatal("refused transition changed local state")
			}
		})
	}
}

func TestNativeGuardRemainsReadableUnderPrivateUmask(t *testing.T) {
	// Managed admission and learning never reached a model: umask 077 made
	// the separate guard UID's configuration unreadable. Isolate the mask
	// from the test runner and exercise the real credential-safe publication.
	cmd := exec.Command(os.Args[0], "-test.run=^TestNativeRunPublishesGuardOnlyExpiringAccess$")
	cmd.Env = append(os.Environ(), "COOP_TEST_NATIVE_PRIVATE_UMASK=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native guard under private mask: %v\n%s", err, output)
	}
}

func TestNativeRunPublishesGuardOnlyExpiringAccess(t *testing.T) {
	if os.Getenv("COOP_TEST_NATIVE_PRIVATE_UMASK") == "1" {
		previous := syscall.Umask(0o077)
		defer syscall.Umask(previous)
	}
	ctx := context.Background()
	cfg := &config.Config{ConfigDir: t.TempDir(), Egress: "open", HomeInBox: "/home/node"}
	staging := t.TempDir()
	if err := os.Chmod(staging, 0700); err != nil {
		t.Fatal(err)
	}
	const private = "inert-real-provider-secret"
	if err := os.WriteFile(filepath.Join(staging, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"`+private+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	account := cfg.ActiveProfile("codex")
	if err := ImportNativeSignIn(ctx, cfg, "codex", account, staging); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	home, err := PrepareNativeHome(ctx, cfg, runtime.Runtime{}, "codex", account, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.WithNativeHomes(map[string]string{"codex": home})
	spec := RunSpec{Agent: "codex", Homes: true, Repo: repo}
	run, err := planNativeRun(ctx, cfg, runtime.Runtime{}, spec)
	if err != nil || run == nil {
		t.Fatal("canonical run unavailable", err)
	}
	if err := run.prepare(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	run.cancel()
	run.workers.Wait() // deterministically test publications without a polling worker
	defer run.close()
	for path, want := range map[string]os.FileMode{
		filepath.Dir(run.dir):                 0o700,
		run.dir:                               0o755,
		run.publicCA:                          0o444,
		filepath.Join(run.dir, "config.json"): 0o444,
		filepath.Join(run.dir, "codex.json"):  0o444,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("native %s mode = %04o, want %04o", filepath.Base(path), got, want)
		}
	}
	public, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil || strings.Contains(string(public), private) || !strings.Contains(string(public), agents.NativeBrokerAccount) {
		t.Fatal("home did not receive a public-only native selector", err)
	}
	if _, err := planNativeRun(ctx, cfg, runtime.Runtime{}, spec); err != nil {
		t.Fatal("public seed was not reusable", err)
	}
	snapshotPath := filepath.Join(run.dir, "codex.json")
	var first networkgateway.NativeAccessSnapshot
	raw, err := os.ReadFile(snapshotPath)
	if err != nil || json.Unmarshal(raw, &first) != nil {
		t.Fatal(err)
	}
	if first.Credential != private || first.Revoked || !first.Expires.After(time.Now()) || first.Expires.After(time.Now().Add(time.Minute)) {
		t.Fatal("incorrect host access lease")
	}
	var plan networkgateway.NativeRunConfig
	raw, err = os.ReadFile(filepath.Join(run.dir, "config.json"))
	if err != nil || json.Unmarshal(raw, &plan) != nil || strings.Contains(string(raw), private) {
		t.Fatal("grant leaked into immutable broker config", err)
	}
	caData, err := os.ReadFile(run.publicCA)
	block, _ := pem.Decode(caData)
	if err != nil || block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("public CA unavailable")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !ca.IsCA {
		t.Fatal("invalid public CA", err)
	}
	args, err := run.agentArgs(compositionArtifactOps{})
	if err != nil || strings.Contains(strings.Join(args, " "), run.dir) || strings.Contains(strings.Join(args, " "), private) {
		t.Fatal("guard custody entered agent arguments", err)
	}
	if err := RemoveNativeAccount(ctx, cfg, "codex", account); err != nil {
		t.Fatal(err)
	}
	ag, _ := agents.Get("codex")
	removed, _, err := readNativeAccount(ctx, cfg, ag, account)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.publish(run.accounts[0], removed, nil); err != nil {
		t.Fatal(err)
	}
	var revoked networkgateway.NativeAccessSnapshot
	raw, err = os.ReadFile(snapshotPath)
	if err != nil || json.Unmarshal(raw, &revoked) != nil {
		t.Fatal(err)
	}
	if !revoked.Revoked || revoked.Credential != "" || revoked.Binding != first.Binding || revoked.Revision <= first.Revision {
		t.Fatal("removal did not revoke the original run binding")
	}
	if profiles, err := EffectiveProfiles(cfg, "codex"); err != nil || len(profiles) != 0 {
		t.Fatal("removed tombstone remained selectable", profiles, err)
	}
}

func TestNativeRunCarriesSelectedAccountDiscoveryProjection(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), Egress: "open", HomeInBox: "/home/node"}
	account := cfg.ActiveProfile("codex")
	seedCanonicalFixture(t, cfg, "codex", account)
	repo := t.TempDir()
	home := scopedFixtureHome(t, cfg, "codex", repo, false)
	cfg = cfg.WithNativeHomes(map[string]string{"codex": home})
	run, err := planNativeRun(t.Context(), cfg, runtime.Runtime{}, RunSpec{Agent: "codex", Homes: true, Repo: repo})
	if err != nil || run == nil {
		t.Fatal(err)
	}
	if err := run.prepare(t.Context(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	run.cancel()
	run.workers.Wait()
	defer run.close()
	data, err := os.ReadFile(filepath.Join(run.dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan networkgateway.NativeRunConfig
	if json.Unmarshal(data, &plan) != nil {
		t.Fatal("invalid native plan")
	}
	found := 0
	for _, selected := range plan.Accounts {
		for _, origin := range selected.Origins {
			for _, line := range origin.Requests {
				if line.AccountResponse == nil {
					continue
				}
				found++
				if origin.Host != "chatgpt.com" || line.Path != "/backend-api/wham/accounts/check" || line.Method != "GET" || origin.AccountHeaders[line.AccountResponse.Header] != "fixture-"+account || origin.ClientAccountHeaders[line.AccountResponse.Header] != agents.NativeBrokerAccount || *line.AccountResponse != (networkgateway.NativeBrokerAccountResponse{Header: "Chatgpt-Account-Id", CollectionKey: "accounts", IDKey: "id", LegacyObjectKey: "account", LegacyIDKey: "account_id", OrderingKey: "account_ordering", DefaultIDKey: "default_account_id"}) {
					t.Fatal("projection lost selected host/public binding")
				}
			}
		}
	}
	if found != 1 {
		t.Fatalf("projection declarations=%d", found)
	}
}

func TestNativeNetworkOptionsMoveOnlyNamespaceConfiguration(t *testing.T) {
	workload, owner, network, err := nativeNetworkOptions([]string{"-e", "KEEP=yes", "--network=bridge", "--add-host", "coop-broker:172.17.0.2", "--publish", "127.0.0.1:3000:3000", "--memory", "1g"})
	if err != nil || network != "bridge" || strings.Join(workload, " ") != "-e KEEP=yes --memory 1g" ||
		strings.Join(owner, " ") != "--add-host coop-broker:172.17.0.2 -p 127.0.0.1:3000:3000" {
		t.Fatalf("workload=%v owner=%v network=%s err=%v", workload, owner, network, err)
	}
	for _, args := range [][]string{{"--network"}, {"-P"}} {
		if _, _, _, err := nativeNetworkOptions(args); err == nil {
			t.Fatal("ambiguous namespace options accepted")
		}
	}
}

func TestNativeRunRejectsCanonicalRollbackAndSameRevisionMutation(t *testing.T) {
	for _, kind := range []string{"rollback", "same-revision"} {
		t.Run(kind, func(t *testing.T) {
			ag, _ := agents.Get("codex")
			first := &accountAuthority{Provider: "codex", Account: "work", Epoch: 5, Revision: 10, Selection: "apikey", Principal: "opaque-api-key",
				Artifacts: map[string][]byte{"auth.json": []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"private-one"}`)}}
			account := &nativeRunAccount{agent: ag, account: "work", record: first}
			run := &nativeRun{runID: strings.Repeat("a", 32), dir: t.TempDir(), accounts: []*nativeRunAccount{account}}
			if err := run.publish(account, first, nil); err != nil {
				t.Fatal(err)
			}
			next := cloneAccountAuthority(first)
			next.Revision++
			next.Artifacts["auth.json"] = []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"private-two"}`)
			if err := run.publish(account, next, nil); err != nil {
				t.Fatal(err)
			}
			bad := first
			if kind == "same-revision" {
				bad = cloneAccountAuthority(next)
				bad.Artifacts["auth.json"] = first.Artifacts["auth.json"]
			}
			if err := run.publish(account, bad, nil); err != nil {
				t.Fatal(err)
			}
			// A later valid-looking record cannot resurrect an invalidated run.
			next.Revision++
			if err := run.publish(account, next, nil); err != nil {
				t.Fatal(err)
			}
			var snapshot networkgateway.NativeAccessSnapshot
			raw, err := os.ReadFile(filepath.Join(run.dir, "codex.json"))
			if err != nil || json.Unmarshal(raw, &snapshot) != nil {
				t.Fatal(err)
			}
			if !snapshot.Revoked || snapshot.Credential != "" || snapshot.Binding.Epoch != 5 {
				t.Fatal("canonical rollback was renewed as a fresh projection")
			}
		})
	}
}

// A first seed writes merged settings, then its receipt, then its markers, so a seed cut short at
// either point is finished by the next launch, while markers that appear without a receipt (planted
// in the home) are still refused.
func TestNativeAuthFirstSeedRecoversFromAnInterruption(t *testing.T) {
	ag, _ := agents.Get("claude")
	native := ag.NativeCredentials()
	// prepareNativeAuth fills in a seed's storage identity, so every call gets its own copy.
	seedHome := func(t *testing.T) (string, agents.NativeBrokerSeed) {
		t.Helper()
		home := filepath.Join(t.TempDir(), "home")
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
		seed, err := native.Broker.Seed("claude-oauth")
		if err != nil {
			t.Fatal(err)
		}
		return home, seed
	}
	for _, tc := range []struct {
		name   string
		remove []string // relative to the home's parent
	}{
		{"after the receipt", []string{"home/.credentials.json"}},
		{"before the receipt", []string{"home/.credentials.json", "auth/seed.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, pristine := seedHome(t)
			seed := pristine
			if err := prepareNativeAuth(t.Context(), home, native, &seed); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.remove {
				if err := os.Remove(filepath.Join(filepath.Dir(home), name)); err != nil {
					t.Fatal(err)
				}
			}
			again := pristine
			if err := prepareNativeAuth(t.Context(), home, native, &again); err != nil {
				t.Fatalf("launch after a seed cut short %s: %v", tc.name, err)
			}
			if !bytes.Contains(mustReadFile(t, filepath.Join(home, ".credentials.json")), []byte(again.Marker)) {
				t.Fatal("the missing selector was not written")
			}
		})
	}
	t.Run("after the receipt, then a family change", func(t *testing.T) {
		home, pristine := seedHome(t)
		seed := pristine
		if err := prepareNativeAuth(t.Context(), home, native, &seed); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(home, ".credentials.json")); err != nil {
			t.Fatal(err)
		}
		apikey, err := native.Broker.Seed("apikey")
		if err != nil {
			t.Fatal(err)
		}
		if err := prepareNativeAuth(t.Context(), home, native, &apikey); err != nil {
			t.Fatalf("family change after a seed cut short: %v", err)
		}
	})
	t.Run("the receipt comes before the markers", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes into a read-only directory")
		}
		home, pristine := seedHome(t)
		auth := filepath.Join(filepath.Dir(home), "auth")
		if err := os.Mkdir(auth, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(auth, ".authority.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(auth, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(auth, 0o700) })
		seed := pristine
		if err := prepareNativeAuth(t.Context(), home, native, &seed); err == nil {
			t.Fatal("the seed succeeded without writing its receipt")
		}
		if _, err := os.Stat(filepath.Join(home, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a marker was written before the receipt, so the next launch would refuse the home")
		}
	})
	t.Run("null settings", func(t *testing.T) {
		home, pristine := seedHome(t)
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("null"), 0o600); err != nil {
			t.Fatal(err)
		}
		seed := pristine
		if err := prepareNativeAuth(t.Context(), home, native, &seed); err == nil {
			t.Fatal("null settings were merged")
		}
	})
	t.Run("planted", func(t *testing.T) {
		home, pristine := seedHome(t)
		planted := []byte(`{"claudeAiOauth":{"accessToken":"inert-planted-access","refreshToken":"inert-planted-refresh"}}`)
		if err := os.WriteFile(filepath.Join(home, ".credentials.json"), planted, 0o600); err != nil {
			t.Fatal(err)
		}
		seed := pristine
		if err := prepareNativeAuth(t.Context(), home, native, &seed); err == nil {
			t.Fatal("a grant planted in a home without a receipt was blessed")
		}
		if !bytes.Equal(mustReadFile(t, filepath.Join(home, ".credentials.json")), planted) {
			t.Fatal("the refused seed changed the planted file")
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(home), "auth", "seed.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a refused seed wrote its receipt")
		}
	})
}
