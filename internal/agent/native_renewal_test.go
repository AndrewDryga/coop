package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// nativeRefreshAnswer points the shared refresh client at a handler for the rest of the test.
func nativeRefreshAnswer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

// Only a 200 answer can hold a new grant, so only a 200 answer is kept, before anything parses it;
// a failed keep still hands the answer back, since publishing the grant keeps it too. Any other
// answer issued nothing and is never kept.
func TestNativeRefreshKeepsOnlyASuccessfulAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		keepErr error
		kept    bool
		ok      bool
	}{
		{"issued", http.StatusOK, `{"access_token":"inert-new-access"}`, nil, true, true},
		{"issued but not kept", http.StatusOK, `{"access_token":"inert-new-access"}`, errors.New("inert disk full"), true, true},
		{"unusable 200 is still kept", http.StatusOK, "not-json", nil, true, true},
		{"gateway page", http.StatusBadGateway, "<html>bad gateway</html>", nil, false, false},
		{"rate limited", http.StatusTooManyRequests, `{"error":"slow_down"}`, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := nativeRefreshAnswer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			kept := ""
			raw, err := requestNativeRefresh(context.Background(), endpoint, "application/json", []byte(`{}`), func(data []byte) error {
				kept = string(data)
				return tc.keepErr
			})
			if (kept != "") != tc.kept || tc.kept && kept != tc.body {
				t.Fatalf("kept %q, want kept=%v", kept, tc.kept)
			}
			if (err == nil) != tc.ok || tc.ok && string(raw) != tc.body {
				t.Fatalf("requestNativeRefresh = %q, %v; want ok=%v", raw, err, tc.ok)
			}
			if !tc.ok && errors.Is(err, ErrRenewalRefused) {
				t.Fatalf("a refusal of this request read as a refused grant: %v", err)
			}
		})
	}
}

// Only a 400 or 401 that names a refused grant asks for a new sign-in, in each provider's shape.
func TestNativeRefreshNamesARefusedGrant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		refused bool
	}{
		{"OAuth (Google, Grok)", http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`, true},
		{"Claude", http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_grant","message":"Invalid refresh token"}}`, true},
		{"OpenAI nested code", http.StatusUnauthorized, `{"error":{"message":"Your refresh token has already been used","type":"invalid_request_error","code":"refresh_token_reused"}}`, true},
		{"top-level code", http.StatusUnauthorized, `{"code":"refresh_token_expired"}`, true},
		{"other OAuth error", http.StatusBadRequest, `{"error":"invalid_request"}`, false},
		{"refused grant code on a 403", http.StatusForbidden, `{"error":"invalid_grant"}`, false},
		{"server error", http.StatusInternalServerError, `{"error":"server_error"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := nativeRefreshAnswer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := requestNativeRefresh(context.Background(), endpoint, "application/json", []byte(`{}`), func([]byte) error {
				t.Fatal("a refusal was kept as if it held a grant")
				return nil
			})
			if err == nil || errors.Is(err, ErrRenewalRefused) != tc.refused || strings.Contains(err.Error(), tc.body) {
				t.Fatalf("requestNativeRefresh error = %v, want refused=%v and no echoed answer", err, tc.refused)
			}
		})
	}
}

// A renewal the provider did not answer, answered with a server error or a rate limit, or whose answer
// was cut off may be asked again later. Any other refusal is not marked retryable.
func TestNativeRefreshNamesAnUnavailableProvider(t *testing.T) {
	for _, tc := range []struct {
		name        string
		handler     http.HandlerFunc
		unavailable bool
	}{
		{"server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, true},
		{"gateway page", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, true},
		{"rate limited", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }, true},
		{"answer cut off", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "64")
			_, _ = io.WriteString(w, `{"access_token":`)
		}, true},
		{"refused grant", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
		}, false},
		{"other request error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_request"}`)
		}, false},
		{"forbidden", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := requestNativeRefresh(context.Background(), nativeRefreshAnswer(t, tc.handler), "application/json", []byte(`{}`), func([]byte) error { return nil })
			if err == nil || errors.Is(err, ErrRenewalUnavailable) != tc.unavailable {
				t.Fatalf("requestNativeRefresh error = %v, want unavailable=%v", err, tc.unavailable)
			}
		})
	}
	t.Run("no answer", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		if _, err := requestNativeRefresh(context.Background(), server.URL, "application/json", []byte(`{}`), func([]byte) error { return nil }); !errors.Is(err, ErrRenewalUnavailable) {
			t.Fatalf("an unreachable provider = %v, want ErrRenewalUnavailable", err)
		}
	})
}

// A caller cancelled after the request left must still get the answer: the provider may already
// have spent the grant it was sent.
func TestNativeRefreshOutlivesItsCallersCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpoint := nativeRefreshAnswer(t, func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, `{"access_token":"inert-new-access"}`)
	})
	raw, err := requestNativeRefresh(ctx, endpoint, "application/json", []byte(`{}`), func([]byte) error { return nil })
	if err != nil || !strings.Contains(string(raw), "inert-new-access") || ctx.Err() == nil {
		t.Fatalf("requestNativeRefresh after cancellation = %q, %v", raw, err)
	}
	if _, err := requestNativeRefresh(ctx, endpoint, "application/json", []byte(`{}`), func([]byte) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("a request cancelled before it left was sent anyway: %v", err)
	}
}

func nativeTestJWT(claims string) string {
	return "x." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".x"
}

// Each provider completes a kept answer without contacting it, and dates the new grant from when
// the answer arrived, never from when Coop got around to it.
func TestNativeCompleteDatesTheGrantFromItsArrival(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	grokEntry, _ := json.Marshal(map[string]string{"key": "inert-old-access", "expires_at": "2026-01-01T00:00:00Z", "create_time": "2026-01-01T00:00:00Z",
		"auth_mode": "oauth", "oidc_issuer": grokIssuer, "oidc_client_id": grokNativeClientID, "principal_type": "user", "principal_id": "inert-principal",
		"user_id": "inert-user", "refresh_token": "inert-old-refresh"})
	for _, tc := range []struct {
		provider string
		files    map[string][]byte
		answer   string
		check    func(map[string][]byte) error
	}{{
		provider: "claude",
		files: map[string][]byte{
			".claude.json":      []byte(`{"oauthAccount":{"accountUuid":"inert-account","organizationUuid":"inert-org"}}`),
			".credentials.json": []byte(`{"claudeAiOauth":{"accessToken":"inert-old-access","refreshToken":"inert-old-refresh","expiresAt":1,"scopes":["user:inference"]}}`),
		},
		answer: `{"access_token":"inert-new-access","refresh_token":"inert-new-refresh","expires_in":3600,"scope":"user:inference"}`,
		check: func(next map[string][]byte) error {
			grant, err := parseClaudeSourceCredential(next[".credentials.json"])
			if err != nil || grant.AccessToken != "inert-new-access" || grant.RefreshToken != "inert-new-refresh" || grant.ExpiresAt != at.Add(time.Hour).UnixMilli() {
				return fmt.Errorf("claude grant = %+v, %v", grant, err)
			}
			return nil
		},
	}, {
		provider: "codex",
		files: map[string][]byte{"auth.json": []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"id_token":%q,"access_token":%q,"refresh_token":"inert-old-refresh","account_id":"inert-account"},"last_refresh":"2026-01-01T00:00:00Z"}`,
			nativeTestJWT(`{"sub":"inert-user"}`), nativeTestJWT(`{"exp":1}`)))},
		answer: fmt.Sprintf(`{"access_token":%q,"refresh_token":"inert-new-refresh"}`, nativeTestJWT(`{"exp":1893456000}`)),
		check: func(next map[string][]byte) error {
			var source codexSourceCredential
			if json.Unmarshal(next["auth.json"], &source) != nil || source.Tokens == nil || source.Tokens.RefreshToken != "inert-new-refresh" ||
				source.LastRefresh != at.Format(time.RFC3339Nano) {
				return fmt.Errorf("codex credential = %s", next["auth.json"])
			}
			return nil
		},
	}, {
		provider: "grok",
		files:    map[string][]byte{"auth.json": []byte(`{"` + grokNativeScope + `":` + string(grokEntry) + `}`)},
		answer:   `{"access_token":"inert-new-access","refresh_token":"inert-new-refresh","expires_in":3600}`,
		check: func(next map[string][]byte) error {
			state, err := inspectGrokNative(next, at)
			if err != nil || state.AccessToken != "inert-new-access" || !state.ExpiresAt.Equal(at.Add(time.Hour)) {
				return fmt.Errorf("grok state = %+v, %v", state, err)
			}
			return nil
		},
	}, {
		provider: "gemini",
		files: map[string][]byte{
			"settings.json":        []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`),
			"google_accounts.json": []byte(`{"active":"inert@example.invalid","old":[]}`),
			"oauth_creds.json":     geminiTestAuthority(),
		},
		answer: `{"access_token":"inert-new-access","expires_in":3600}`,
		check: func(next map[string][]byte) error {
			state, err := inspectGeminiNative(next, at)
			if err != nil || state.AccessToken != "inert-new-access" || !state.ExpiresAt.Equal(at.Add(time.Hour)) {
				return fmt.Errorf("gemini state = %+v, %v", state, err)
			}
			return nil
		},
	}} {
		t.Run(tc.provider, func(t *testing.T) {
			ag, ok := Get(tc.provider)
			if !ok || ag.NativeCredentials().Complete == nil {
				t.Fatalf("%s declares no Complete", tc.provider)
			}
			next, err := ag.NativeCredentials().Complete(tc.files, []byte(tc.answer), at)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.check(next); err != nil {
				t.Fatal(err)
			}
			if _, err := ag.NativeCredentials().Complete(tc.files, []byte("not-json"), at); err == nil {
				t.Fatal("an unreadable answer completed")
			}
		})
	}
}
