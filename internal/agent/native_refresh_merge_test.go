package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A renewal edits only the fields it renews: a response that leaves out the refresh token keeps the
// stored one, and fields Coop does not model survive.
func TestClaudeRefreshMergeKeepsWhatTheGrantDoesNotReplace(t *testing.T) {
	stored := []byte(`{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"source-refresh","expiresAt":1,` +
		`"scopes":["user:inference","user:profile"],"subscriptionType":"max","futureField":"kept"},"futureTopField":"kept"}`)
	source, err := parseClaudeSourceCredential(stored)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_800_000_000, 0)
	merged, granted, err := mergeClaudeCredentialRefresh(stored, source, claudeRefreshResponse{AccessToken: "renewed-access", ExpiresIn: 3600}, at)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseClaudeSourceCredential(merged)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "renewed-access" || got.RefreshToken != "source-refresh" || got.ExpiresAt != at.Add(time.Hour).UnixMilli() {
		t.Fatalf("merged = %+v", got)
	}
	if granted.AccessToken != "renewed-access" || strings.Join(granted.Scopes, " ") != "user:inference user:profile" {
		t.Fatalf("granted = %+v", granted)
	}
	for _, kept := range []string{`"subscriptionType":"max"`, `"futureField":"kept"`, `"futureTopField":"kept"`} {
		if !strings.Contains(string(merged), kept) {
			t.Errorf("the merge dropped %s: %s", kept, merged)
		}
	}
	rotated, _, err := mergeClaudeCredentialRefresh(stored, source, claudeRefreshResponse{AccessToken: "a", RefreshToken: "rotated", ExpiresIn: 60}, at)
	if err != nil || !strings.Contains(string(rotated), `"refreshToken":"rotated"`) {
		t.Fatalf("a rotated refresh token was not kept: %s, %v", rotated, err)
	}
	if _, _, err := mergeClaudeCredentialRefresh(stored, source, claudeRefreshResponse{ExpiresIn: 60}, at); err == nil {
		t.Fatal("a response without an access token was merged")
	}
}

func TestGrokRefreshMergeKeepsWhatTheGrantDoesNotReplace(t *testing.T) {
	stored := json.RawMessage(`{"key":"old-access","refresh_token":"source-refresh","expires_at":"2000-01-01T00:00:00Z","future_field":"kept"}`)
	at := time.Unix(1_800_000_000, 0)
	merged, expires, err := mergeGrokCredentialRefresh(stored, grokRefreshResponse{AccessToken: "renewed-access", ExpiresIn: 21600}, at)
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]string
	if err := json.Unmarshal(merged, &entry); err != nil {
		t.Fatal(err)
	}
	if entry["key"] != "renewed-access" || entry["refresh_token"] != "source-refresh" || entry["future_field"] != "kept" ||
		!expires.Equal(at.Add(6*time.Hour)) || entry["expires_at"] != at.Add(6*time.Hour).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("merged = %v, expires %v", entry, expires)
	}
	rotated, _, err := mergeGrokCredentialRefresh(stored, grokRefreshResponse{AccessToken: "a", RefreshToken: "rotated", ExpiresIn: 60}, at)
	if err != nil || !strings.Contains(string(rotated), `"refresh_token":"rotated"`) {
		t.Fatalf("a rotated refresh token was not kept: %s, %v", rotated, err)
	}
	if _, _, err := mergeGrokCredentialRefresh(stored, grokRefreshResponse{ExpiresIn: 60}, at); err == nil {
		t.Fatal("a response without an access token was merged")
	}
}
