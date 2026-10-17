package box

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/runtime"
	"golang.org/x/sys/unix"
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

// nativeRenewalRun is a run serving one Gemini account whose access expires after the given time, as
// prepare leaves it: the access published and the account's worker started. Each renewal asks answer
// instead of the provider: it returns how long the access it issues lasts, or the error the renewal
// fails with. Call it inside a synctest bubble, so the worker's ticks and waits run on the bubble's
// clock. It returns the run and a reader for the guard's snapshot.
func nativeRenewalRun(t *testing.T, expires time.Duration, answer func() (time.Duration, error)) (*nativeRun, func() networkgateway.NativeAccessSnapshot) {
	t.Helper()
	base, _ := agents.Get("gemini")
	native := base.NativeCredentials()
	complete := native.Complete
	native.Renew = func(_ context.Context, files map[string][]byte, _ time.Time, retain func([]byte) error) (map[string][]byte, error) {
		lifetime, err := answer()
		if err != nil {
			return nil, err
		}
		issued := fmt.Appendf(nil, `{"access_token":"inert-renewed-access","expires_in":%d}`, int64(lifetime/time.Second))
		if err := retain(issued); err != nil {
			return nil, err
		}
		return complete(files, issued, time.Now())
	}
	ag := nativeRenewalFixtureAgent{Agent: base, native: native}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	record := signInInertGemini(t, cfg, ag, "inert-first-access", expires)
	account := &nativeRunAccount{agent: ag, account: "work", record: record}
	run := &nativeRun{cfg: cfg, runID: strings.Repeat("a", 32), dir: t.TempDir(), accounts: []*nativeRunAccount{account}}
	if err := run.publish(account, record, nil); err != nil {
		t.Fatal(err)
	}
	lifetime, cancel := context.WithCancel(t.Context())
	run.cancel = cancel
	run.workers.Go(func() { run.serve(lifetime, account) })
	t.Cleanup(func() { _ = run.close() })
	synctest.Wait()
	return run, func() networkgateway.NativeAccessSnapshot {
		t.Helper()
		var snapshot networkgateway.NativeAccessSnapshot
		if err := json.Unmarshal(mustReadFile(t, filepath.Join(run.dir, "gemini.json")), &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
}

// signInInertGemini signs the Gemini account "work" in on the host, as a completed coop login does,
// with an access that expires after the given time. Each sign-in starts a new epoch of the account.
func signInInertGemini(t *testing.T, cfg *config.Config, ag agents.Agent, access string, expires time.Duration) *accountAuthority {
	t.Helper()
	record, err := normalizeNativeAccount(ag, map[string][]byte{
		"settings.json":        []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`),
		"google_accounts.json": []byte(`{"active":"inert@example.invalid","old":[]}`),
		"oauth_creds.json": fmt.Appendf(nil, `{"access_token":"%s","refresh_token":"inert-refresh-grant","expiry_date":%d,"token_type":"Bearer"}`,
			access, time.Now().Add(expires).UnixMilli()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record, _, err = replaceAccountAuthority(t.Context(), cfg, nativeAccountSpec(ag), "work", record); err != nil {
		t.Fatal(err)
	}
	return record
}

// holdNativeAccount takes the run's account lock as another Coop process would, until the test ends
// or the returned file is closed.
func holdNativeAccount(t *testing.T, run *nativeRun) *os.File {
	t.Helper()
	holder, err := os.OpenFile(filepath.Join(run.cfg.ConfigDir, "gemini", "credentials", "work", ".authority.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return holder
}

// A renewal the provider could not answer keeps the box on the access it already holds, and is tried
// again 5 s later, then 10 s after that; the ticks in between do not ask the provider. Once a retry
// succeeds, the box gets the renewed access.
func TestNativeBrokerRetriesARenewalTheProviderCouldNotAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		_, snapshot := nativeRenewalRun(t, 90*time.Second, func() (time.Duration, error) {
			if calls.Add(1) <= 2 {
				return 0, fmt.Errorf("inert outage: %w", agents.ErrRenewalUnavailable)
			}
			return time.Hour, nil
		})
		for i, want := range []struct {
			calls  int32
			access string
		}{
			{1, "inert-first-access"},   // 5 s: the renewal fails
			{2, "inert-first-access"},   // 10 s: so does its retry, 5 s later
			{2, "inert-first-access"},   // 15 s: the next retry waits 10 s
			{3, "inert-renewed-access"}, // 20 s: and succeeds
			{3, "inert-renewed-access"}, // 25 s: the renewed access needs no renewal
		} {
			time.Sleep(5 * time.Second)
			synctest.Wait()
			got := snapshot()
			if calls.Load() != want.calls || got.Revoked || got.Credential != want.access {
				t.Fatalf("after %d s: %d provider calls, snapshot revoked %v serving %q; want %d calls serving %q",
					5*(i+1), calls.Load(), got.Revoked, got.Credential, want.calls, want.access)
			}
		}
	})
}

// A renewal that gets through starts the backoff over: when the next renewal fails, it is tried again
// 5 s later, not after the longer wait the earlier outage had reached.
func TestNativeBrokerResetsTheBackoffAfterARenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		_, snapshot := nativeRenewalRun(t, 90*time.Second, func() (time.Duration, error) {
			if calls.Add(1) == 3 {
				// Renewal is due with under 2 minutes left, so this access is due by the next tick, not at once.
				return 2*time.Minute + time.Second, nil
			}
			return 0, fmt.Errorf("inert outage: %w", agents.ErrRenewalUnavailable)
		})
		for i, want := range []struct {
			calls  int32
			access string
		}{
			{1, "inert-first-access"},   // 5 s: the renewal fails
			{2, "inert-first-access"},   // 10 s: so does its retry, 5 s later
			{2, "inert-first-access"},   // 15 s: the next retry waits 10 s
			{3, "inert-renewed-access"}, // 20 s: and succeeds
			{4, "inert-renewed-access"}, // 25 s: renewing that access fails
			{5, "inert-renewed-access"}, // 30 s: and is tried again 5 s later, not 20
			{5, "inert-renewed-access"}, // 35 s: the next retry waits 10 s
		} {
			time.Sleep(5 * time.Second)
			synctest.Wait()
			got := snapshot()
			if calls.Load() != want.calls || got.Revoked || got.Credential != want.access {
				t.Fatalf("after %d s: %d provider calls, snapshot revoked %v serving %q; want %d calls serving %q",
					5*(i+1), calls.Load(), got.Revoked, got.Credential, want.calls, want.access)
			}
		}
	})
}

// An outage that outlasts the access ends the run's access when the access expires, not before, and
// asks the provider only on the backoff schedule, never on every tick.
func TestNativeBrokerRevokesAnAccessThatExpiresDuringAnOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		expiry := time.Now().Add(90 * time.Second)
		_, snapshot := nativeRenewalRun(t, 90*time.Second, func() (time.Duration, error) {
			calls.Add(1)
			return 0, fmt.Errorf("inert outage: %w", agents.ErrRenewalUnavailable)
		})
		attempts := map[int]bool{5: true, 10: true, 20: true, 40: true, 80: true} // 5, 10, 20 and 40 s apart; the next would be past the expiry
		var want int32
		for elapsed := 5; elapsed < 90; elapsed += 5 {
			time.Sleep(5 * time.Second)
			synctest.Wait()
			if attempts[elapsed] {
				want++
			}
			got := snapshot()
			if calls.Load() != want || got.Revoked || got.Credential != "inert-first-access" || got.Expires.After(expiry) {
				t.Fatalf("after %d s: %d provider calls (want %d), snapshot revoked %v serving %q until %v; want the first access until it expires",
					elapsed, calls.Load(), want, got.Revoked, got.Credential, got.Expires)
			}
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		expired := snapshot()
		if !expired.Revoked || expired.Credential != "" {
			t.Fatal("an expired access was still served")
		}
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		if calls.Load() != 5 || snapshot().Revision != expired.Revision {
			t.Fatalf("the run kept working after its access expired: %d provider calls", calls.Load())
		}
	})
}

// A refused grant ends the run's access at once and is never sent again.
func TestNativeBrokerRevokesARefusedGrantWithoutRetrying(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		_, snapshot := nativeRenewalRun(t, 90*time.Second, func() (time.Duration, error) {
			calls.Add(1)
			return 0, agents.ErrRenewalRefused
		})
		time.Sleep(5 * time.Second)
		synctest.Wait()
		refused := snapshot()
		if calls.Load() != 1 || !refused.Revoked || refused.Credential != "" {
			t.Fatalf("refused grant: %d provider calls, snapshot revoked %v", calls.Load(), refused.Revoked)
		}
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		if calls.Load() != 1 || snapshot().Revision != refused.Revision {
			t.Fatalf("a refused grant was retried: %d provider calls", calls.Load())
		}
	})
}

// While a failed renewal waits to be tried again, the ticks in between still read the account, so a
// removal on the host ends the run's access at the next tick, not at the next renewal attempt.
func TestNativeBrokerRevokesARemovedAccountWhileARenewalWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		run, snapshot := nativeRenewalRun(t, 90*time.Second, func() (time.Duration, error) {
			calls.Add(1)
			return 0, fmt.Errorf("inert outage: %w", agents.ErrRenewalUnavailable)
		})
		time.Sleep(10 * time.Second) // renewals fail at 5 s and 10 s; the next waits until 20 s
		synctest.Wait()
		if err := RemoveNativeAccount(t.Context(), run.cfg, "gemini", "work"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if removed := snapshot(); calls.Load() != 2 || !removed.Revoked || removed.Credential != "" {
			t.Fatalf("after a host removal: %d provider calls, snapshot revoked %v; want 2 calls and a revoked snapshot",
				calls.Load(), removed.Revoked)
		}
	})
}

// A host sign-in while a failed renewal waits starts a new epoch of the account, and the next tick ends
// the run's access. The new sign-in's access is valid, but this run serves only the epoch it started
// with.
func TestNativeBrokerRevokesANewSignInWhileARenewalWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		run, snapshot := nativeRenewalRun(t, 90*time.Second, func() (time.Duration, error) {
			calls.Add(1)
			return 0, fmt.Errorf("inert outage: %w", agents.ErrRenewalUnavailable)
		})
		time.Sleep(10 * time.Second) // renewals fail at 5 s and 10 s; the next waits until 20 s
		synctest.Wait()
		if signedIn := signInInertGemini(t, run.cfg, run.accounts[0].agent, "inert-second-access", time.Hour); signedIn.Epoch == run.accounts[0].record.Epoch {
			t.Fatalf("the host sign-in kept epoch %d", signedIn.Epoch)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got := snapshot(); calls.Load() != 2 || !got.Revoked || got.Credential != "" {
			t.Fatalf("after a host sign-in: %d provider calls, snapshot revoked %v serving %q; want 2 calls and a revoked snapshot",
				calls.Load(), got.Revoked, got.Credential)
		}
	})
}

// Another process holding the account for longer than an attempt waits is a passing failure too: the
// run keeps serving the access it holds, past the lease it had when the account became busy, and goes
// on once the account is free. No renewal is due, so the provider is never asked.
func TestNativeBrokerKeepsServingWhileTheAccountIsBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		start := time.Now()
		run, snapshot := nativeRenewalRun(t, time.Hour, func() (time.Duration, error) {
			calls.Add(1)
			return time.Hour, nil
		})
		holder := holdNativeAccount(t, run)
		time.Sleep(35 * time.Second) // the first tick's attempt waits 30 s for the account, then gives up
		synctest.Wait()
		busy := snapshot()
		if busy.Revoked || busy.Credential != "inert-first-access" || !busy.Expires.After(start.Add(time.Minute)) {
			t.Fatalf("while the account was busy: snapshot revoked %v serving %q until %v; want the held access on a renewed lease",
				busy.Revoked, busy.Credential, busy.Expires)
		}
		if err := holder.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if free := snapshot(); free.Revoked || free.Credential != "inert-first-access" || free.Revision <= busy.Revision || calls.Load() != 0 {
			t.Fatalf("once the account was free: snapshot revoked %v at revision %d, %d provider calls", free.Revoked, free.Revision, calls.Load())
		}
	})
}

// An account another process holds for more than 2 minutes after the last read ends the run's access,
// although the access itself lasts an hour: a run that cannot read the account cannot see a host
// sign-in or removal either.
func TestNativeBrokerRevokesAnAccountThatStaysBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		run, snapshot := nativeRenewalRun(t, time.Hour, func() (time.Duration, error) {
			calls.Add(1)
			return time.Hour, nil
		})
		holder := holdNativeAccount(t, run)
		time.Sleep(95 * time.Second) // attempts wait 30 s each for the account and give up at 35, 65 and 95 s
		synctest.Wait()
		if busy := snapshot(); busy.Revoked || busy.Credential != "inert-first-access" {
			t.Fatalf("95 s after the last read: snapshot revoked %v serving %q; want the held access", busy.Revoked, busy.Credential)
		}
		time.Sleep(30 * time.Second) // the attempt that gives up at 125 s is past the 2 minutes
		synctest.Wait()
		revoked := snapshot()
		if !revoked.Revoked || revoked.Credential != "" {
			t.Fatalf("125 s after the last read: snapshot revoked %v serving %q; want a revoked snapshot", revoked.Revoked, revoked.Credential)
		}
		if err := holder.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if calls.Load() != 0 || snapshot().Revision != revoked.Revision {
			t.Fatalf("the run went on once the account was free: %d provider calls", calls.Load())
		}
	})
}

// Only a failure that may pass is retried, and every error a failure joins must pass: a refused grant,
// an unusable kept answer or a custody failure that comes with an outage is not retried.
func TestNativeBrokerRetriesOnlyPassingRenewalFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"no answer", agents.ErrRenewalUnavailable, true},
		{"server error", fmt.Errorf("%w (HTTP 503)", agents.ErrRenewalUnavailable), true},
		{"server error settled with nothing left", errors.Join(nil, fmt.Errorf("%w (HTTP 503)", agents.ErrRenewalUnavailable)), true},
		{"busy account", errAccountAuthorityBusy, true},
		{"attempt timed out", context.DeadlineExceeded, true},
		{"attempt timed out with its intent cleared", errors.Join(context.DeadlineExceeded, nil), true},
		{"refused grant", agents.ErrRenewalRefused, false},
		{"unusable kept answer", errors.Join(errAccountRenewalUncertain, fmt.Errorf("%w: response incomplete", agents.ErrRenewalUnavailable)), false},
		{"custody failure with an outage", errors.Join(errors.New("canonical credential lock changed"), agents.ErrRenewalUnavailable), false},
		{"timeout whose intent could not be cleared", errors.Join(context.DeadlineExceeded, errors.New("canonical credential lock changed")), false},
		{"needs sign-in", errors.New("account needs host sign-in"), false},
		{"run closing", context.Canceled, false},
	} {
		if got := renewalRetryable(tc.err); got != tc.retry {
			t.Errorf("%s: retryable = %v, want %v", tc.name, got, tc.retry)
		}
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
