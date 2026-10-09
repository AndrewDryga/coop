package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A valid rotation is current upstream authority even when its access token cannot cover
// this caller's deadline. Keep it before refusing the turn so the next refresh can recover.
func TestCodexCredentialRenewalKeepsARotationItCannotUse(t *testing.T) {
	for _, rotate := range []bool{true, false} {
		t.Run(fmt.Sprintf("rotate=%v", rotate), func(t *testing.T) {
			now := time.Now()
			jwt := func(expiry time.Time) string {
				return "x." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiry.Unix()))) + ".x"
			}
			wantedRefresh := "source-refresh"
			if rotate {
				wantedRefresh = "rotated-refresh"
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				var request codexRefreshRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				want := "source-refresh"
				if call > 1 {
					want = wantedRefresh
				}
				if request.RefreshToken != want {
					t.Errorf("refresh input=%q, want %q", request.RefreshToken, want)
				}
				response := codexRefreshResponse{IDToken: "new-identity", AccessToken: jwt(now.Add(time.Minute))}
				if rotate {
					response.RefreshToken = wantedRefresh
				}
				if call > 1 {
					response.AccessToken = jwt(now.Add(4 * time.Hour))
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", server.URL)
			profile := t.TempDir()
			path := filepath.Join(profile, "auth.json")
			mustWrite(t, path, fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"id_token":"old-identity","access_token":%q,"refresh_token":"source-refresh","account_id":"account","unknown":"preserved"},"last_refresh":"2026-07-15T00:00:00Z","unknown":"preserved"}`, jwt(now.Add(-time.Hour))))
			if err := renewCodexCredential(profile, now.Add(time.Hour)); err == nil {
				t.Fatal("short grant reported ready")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved codexSourceCredential
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Tokens == nil || saved.Tokens.RefreshToken != wantedRefresh || saved.Tokens.AccessToken != jwt(now.Add(time.Minute)) || saved.Tokens.IDToken != "new-identity" || saved.LastRefresh == "2026-07-15T00:00:00Z" {
				t.Fatal("discarded issued authority before refusing the caller's deadline")
			}
			if rotate && strings.Contains(string(data), "source-refresh") {
				t.Fatal("old rotated refresh authority retained")
			}
			if strings.Count(string(data), "preserved") != 2 {
				t.Fatal("unknown native fields were discarded")
			}
			if err := renewCodexCredential(profile, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatalf("issuer calls=%d, want2", calls.Load())
			}
		})
	}
}

func TestCodexCredentialRenewalRejectsUnusableGrantBeforePublication(t *testing.T) {
	for _, access := range []string{"", "opaque", "x.e30.x", "x.eyJleHAiOi0xfQ.x"} {
		t.Run(access, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(codexRefreshResponse{AccessToken: access})
			}))
			defer server.Close()
			t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", server.URL)
			profile := t.TempDir()
			path := filepath.Join(profile, "auth.json")
			body := `{"auth_mode":"chatgpt","tokens":{"id_token":"identity","access_token":"expired","refresh_token":"source-refresh"},"last_refresh":"2026-07-15T00:00:00Z"}`
			mustWrite(t, path, body)
			if err := renewCodexCredential(profile, time.Now().Add(time.Hour)); err == nil {
				t.Fatal("unusable grant reported ready")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != body {
				t.Fatal("unusable nonrotating grant changed original authority")
			}
		})
	}
}
