package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// grokCredentialEntry is one stored Grok login in the pinned client's shape, plus a field coop does
// not model, which a renewal must carry through untouched.
func grokCredentialEntry(issuer, client, access string, expiresAt time.Time, refresh string) map[string]string {
	return map[string]string{
		"key": access, "refresh_token": refresh, "expires_at": expiresAt.UTC().Format(time.RFC3339Nano),
		"create_time": expiresAt.Add(-6 * time.Hour).UTC().Format(time.RFC3339Nano), "auth_mode": "oidc",
		"oidc_issuer": issuer, "oidc_client_id": client, "principal_id": "principal", "principal_type": "user",
		"user_id": "user", "team_id": "team", "email": "someone@example.com", "future_field": "kept",
	}
}

// grokCredentialFile is a stored auth.json holding the entries, keyed as the client keys them.
func grokCredentialFile(entries ...map[string]string) string {
	document := map[string]map[string]string{}
	for _, entry := range entries {
		document[entry["oidc_issuer"]+"::"+entry["oidc_client_id"]] = entry
	}
	encoded, _ := json.Marshal(document)
	return string(encoded)
}

func grokCredentialJSON(issuer, access string, expiresAt time.Time, refresh string) string {
	return grokCredentialFile(grokCredentialEntry(issuer, "client", access, expiresAt, refresh))
}

// grokRefreshServer answers like auth.x.ai's token endpoint and counts what it was asked.
func grokRefreshServer(t *testing.T, requests *atomic.Int32, respond func(http.ResponseWriter, *http.Request)) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("GROK_REFRESH_TOKEN_URL_OVERRIDE", server.URL)
}

// A session turn renews the host's Grok login once, with exactly the form the pinned 1.0.25 client
// sends (captured against a logging issuer), and writes the rotated pair back where the client
// reads it. Concurrent turns share one refresh: they wait on the client's own lock, then adopt it.
func TestGrokCredentialRenewalMatchesThePinnedClientAndIsSerialized(t *testing.T) {
	var requests atomic.Int32
	grokRefreshServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse refresh form: %v", err)
		}
		want := map[string]string{
			"grant_type": "refresh_token", "refresh_token": "source-refresh", "client_id": "client",
			"principal_type": "user", "principal_id": "principal",
		}
		if len(r.PostForm) != len(want) || r.Method != http.MethodPost ||
			r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("refresh request = %s %v %q, want the client's five form fields", r.Method, r.PostForm, r.Header.Get("Content-Type"))
		}
		for field, value := range want {
			if got := r.PostForm.Get(field); got != value {
				t.Errorf("refresh field %s = %q, want %q", field, got, value)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "renewed-access", "refresh_token": "rotated-refresh", "expires_in": 21600, "token_type": "Bearer",
		})
	})
	profile := t.TempDir()
	path := filepath.Join(profile, "auth.json")
	mustWrite(t, path, grokCredentialJSON(grokIssuer, "expired-access", time.Now().Add(-time.Hour), "source-refresh"))

	prepare := mustLiveCredentials(t, grokAgent{}).Prepare
	if prepare == nil {
		t.Fatal("grok declares no credential preparation")
	}
	deadline := time.Now().Add(time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- prepare(profile, deadline)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("renew credential: %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("refresh requests = %d, want 1", got)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("renewed credential mode = %v, %v; want 0600 like the client writes", info, err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("renewal did not take the client's lock file: %v", err)
	}
	credentials, ok := readGrokCredentials(profile)
	if !ok || len(credentials) != 1 {
		t.Fatalf("renewed credential does not decode: %v", credentials)
	}
	for _, credential := range credentials {
		created, _ := time.Parse(time.RFC3339Nano, credential.CreateTime)
		expires, _ := time.Parse(time.RFC3339Nano, credential.ExpiresAt)
		if credential.Key != "renewed-access" || credential.RefreshToken != "rotated-refresh" ||
			time.Since(created) > time.Minute || expires.Sub(created) != 6*time.Hour {
			t.Fatalf("renewed entry = %+v, want the client's mapping: key, rotated refresh, create_time now, expiry +expires_in", credential)
		}
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"future_field":"kept"`) || !strings.Contains(string(data), `"email":"someone@example.com"`) {
		t.Fatalf("renewal dropped fields coop does not model: %s", data)
	}
	// The point of the renewal: the access-only copy a session projects now outlives the turn —
	// and no copy of the spent refresh token survives, in the login or in what the box gets.
	projected, err := projectGrokCredential(data)
	if err != nil || strings.Contains(string(projected), "rotated-refresh") {
		t.Fatalf("projection = %s, %v; want the fresh access token without refresh authority", projected, err)
	}
	if strings.Contains(string(data), "source-refresh") || strings.Contains(string(projected), "source-refresh") {
		t.Fatalf("the spent refresh token survived the rotation:\n%s\n%s", data, projected)
	}
	box := t.TempDir()
	mustWrite(t, filepath.Join(box, "auth.json"), string(projected))
	if got := grokCredentialPortability(box, deadline); got != CredentialPortable {
		t.Fatalf("projected portability = %v, want portable", got)
	}
}

// Refresh tokens rotate, so a login that already outlives the turn is never refreshed.
func TestGrokCredentialRenewalLeavesAFreshCredentialAlone(t *testing.T) {
	var requests atomic.Int32
	grokRefreshServer(t, &requests, func(http.ResponseWriter, *http.Request) {})
	profile := t.TempDir()
	body := grokCredentialJSON(grokIssuer, "live-access", time.Now().Add(5*time.Hour), "source-refresh")
	mustWrite(t, filepath.Join(profile, "auth.json"), body)
	if err := renewGrokCredential(profile, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(profile, "auth.json")); requests.Load() != 0 || string(data) != body {
		t.Fatalf("a fresh login was refreshed (%d requests):\n%s", requests.Load(), data)
	}
}

// The profile is mounted read-write into boxes, so the issuer it names is the agent's to write.
// The host refreshes only against the pinned endpoint, and only a login from its issuer.
func TestGrokCredentialRenewalRefusesAnotherIssuer(t *testing.T) {
	var requests atomic.Int32
	grokRefreshServer(t, &requests, func(http.ResponseWriter, *http.Request) {})
	profile := t.TempDir()
	body := grokCredentialJSON("https://attacker.example", "expired-access", time.Now().Add(-time.Hour), "source-refresh")
	mustWrite(t, filepath.Join(profile, "auth.json"), body)
	err := renewGrokCredential(profile, time.Now().Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "issuer other than https://auth.x.ai") || requests.Load() != 0 {
		t.Fatalf("renewal of another issuer's login = %v after %d requests; want a refusal before any", err, requests.Load())
	}
	if strings.Contains(err.Error(), "attacker.example") {
		t.Fatalf("the refusal repeats the box-writable issuer: %v", err)
	}

	// Beside a login from the pinned issuer, the other issuer's entry is skipped, not a refusal.
	mustWrite(t, filepath.Join(profile, "auth.json"), grokCredentialFile(
		grokCredentialEntry("https://attacker.example", "client", "expired-access", time.Now().Add(-time.Hour), "foreign-refresh"),
		grokCredentialEntry(grokIssuer, "client", "expired-access", time.Now().Add(-time.Hour), "source-refresh")))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.ParseForm() != nil || r.PostForm.Get("refresh_token") != "source-refresh" {
			t.Errorf("refreshed %v, want only the pinned issuer's login", r.PostForm)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "renewed-access", "refresh_token": "rotated-refresh", "expires_in": 21600})
	}))
	defer server.Close()
	t.Setenv("GROK_REFRESH_TOKEN_URL_OVERRIDE", server.URL)
	if err := renewGrokCredential(profile, time.Now().Add(time.Hour)); err != nil || requests.Load() != 1 {
		t.Fatalf("renewal beside a foreign entry = %v after %d requests, want one refresh", err, requests.Load())
	}
	if data, _ := os.ReadFile(filepath.Join(profile, "auth.json")); !strings.Contains(string(data), "foreign-refresh") {
		t.Fatalf("the foreign entry was touched:\n%s", data)
	}
}

// Two logins from the pinned issuer each spend their own refresh token. When the second refresh
// fails after the first rotated, the first rotation is still written — upstream its old token is
// already gone — and the failure is reported.
func TestGrokCredentialRenewalKeepsARotationWhenAnotherEntryFails(t *testing.T) {
	var requests atomic.Int32
	grokRefreshServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("client_id") == "failing" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "renewed-access", "refresh_token": "rotated-refresh", "expires_in": 21600})
	})
	profile := t.TempDir()
	path := filepath.Join(profile, "auth.json")
	// Entries renew in key order, so "a-working" rotates before "failing" fails.
	mustWrite(t, path, grokCredentialFile(
		grokCredentialEntry(grokIssuer, "a-working", "expired-access", time.Now().Add(-time.Hour), "working-refresh"),
		grokCredentialEntry(grokIssuer, "failing", "expired-access", time.Now().Add(-time.Hour), "failing-refresh")))
	err := renewGrokCredential(profile, time.Now().Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "needs sign-in") || requests.Load() != 2 {
		t.Fatalf("renewal with one failing entry = %v after %d requests, want both tried and the failure reported", err, requests.Load())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "rotated-refresh") || strings.Contains(string(data), "working-refresh") {
		t.Fatalf("the rotation that succeeded was not written:\n%s", data)
	}
	if !strings.Contains(string(data), "failing-refresh") {
		t.Fatalf("the failing entry was changed:\n%s", data)
	}
}

// The client holds its lock across a refresh and its recovery; a session turn waits for it, but
// not forever — a wedged holder costs this turn a clear error, never a hang.
func TestGrokCredentialRenewalGivesUpOnAHeldLock(t *testing.T) {
	var requests atomic.Int32
	grokRefreshServer(t, &requests, func(http.ResponseWriter, *http.Request) {})
	profile := t.TempDir()
	mustWrite(t, filepath.Join(profile, "auth.json"), grokCredentialJSON(grokIssuer, "expired-access", time.Now().Add(-time.Hour), "source-refresh"))
	holder, err := os.OpenFile(filepath.Join(profile, "auth.json.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = renewGrokCredential(profile, time.Now().Add(300*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "being refreshed by another process") || requests.Load() != 0 {
		t.Fatalf("renewal under a held lock = %v after %d requests", err, requests.Load())
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("renewal waited %v past its deadline for a held lock", waited)
	}
}

// A refused or failed refresh leaves the stored login exactly as it was, and says what to do.
func TestGrokCredentialRenewalFailurePreservesSource(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusBadRequest: "needs sign-in", http.StatusUnauthorized: "needs sign-in",
		http.StatusInternalServerError: "HTTP 500", http.StatusFound: "HTTP 302",
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int32
			grokRefreshServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusFound {
					http.Redirect(w, r, "http://127.0.0.1:9/elsewhere", status) // never followed
					return
				}
				w.WriteHeader(status)
			})
			profile := t.TempDir()
			body := grokCredentialJSON(grokIssuer, "expired-access", time.Now().Add(-time.Hour), "source-refresh")
			mustWrite(t, filepath.Join(profile, "auth.json"), body)
			err := renewGrokCredential(profile, time.Now().Add(time.Hour))
			if err == nil || !strings.Contains(err.Error(), want) || requests.Load() != 1 {
				t.Fatalf("renewal = %v after %d requests, want one request and %q", err, requests.Load(), want)
			}
			if data, _ := os.ReadFile(filepath.Join(profile, "auth.json")); string(data) != body {
				t.Fatalf("a failed refresh changed the stored login:\n%s", data)
			}
		})
	}
}

// A response that omits refresh_token means the stored one still stands; a grant too short for
// the turn is still written — the old refresh token is spent — and reported.
func TestGrokCredentialRenewalKeepsWhatTheGrantDoesNotReplace(t *testing.T) {
	var requests atomic.Int32
	expiresIn := 21600
	grokRefreshServer(t, &requests, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "renewed-access", "expires_in": expiresIn})
	})
	profile := t.TempDir()
	path := filepath.Join(profile, "auth.json")
	mustWrite(t, path, grokCredentialJSON(grokIssuer, "expired-access", time.Now().Add(-time.Hour), "source-refresh"))
	if err := renewGrokCredential(profile, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	credentials, _ := readGrokCredentials(profile)
	for _, credential := range credentials {
		if credential.Key != "renewed-access" || credential.RefreshToken != "source-refresh" {
			t.Fatalf("renewed entry = %+v, want the new access token and the unrotated refresh token", credential)
		}
	}

	expiresIn = 60
	mustWrite(t, path, grokCredentialJSON(grokIssuer, "expired-access", time.Now().Add(-time.Hour), "source-refresh"))
	err := renewGrokCredential(profile, time.Now().Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "expires before the turn deadline") {
		t.Fatalf("a one-minute grant for an hour-long turn = %v, want it reported", err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "renewed-access") {
		t.Fatalf("the short grant was not persisted, losing the only working login:\n%s", data)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("refresh requests = %d, want 2", got)
	}
}
