package nativeauth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// Inert host-authority fixtures, never public broker seeds. Distant expiry
// keeps unrelated assembly tests from invoking a provider renewal endpoint.
func canonicalFixtureJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func Files(t *testing.T, provider, account string) map[string][]byte {
	t.Helper()
	access, refresh, identity := "ACCESS_CANARY-"+provider+"-"+account, "REFRESH_CANARY-"+provider+"-"+account, "fixture-"+account
	switch provider {
	case "claude":
		return map[string][]byte{
			".credentials.json": canonicalFixtureJSON(t, map[string]any{"claudeAiOauth": map[string]any{"accessToken": access, "refreshToken": refresh, "expiresAt": int64(4102444800000), "scopes": []string{"user:inference", "account:read"}}}),
			".claude.json":      canonicalFixtureJSON(t, map[string]any{"oauthAccount": map[string]any{"accountUuid": identity, "organizationUuid": "fixture-organization"}}),
		}
	case "codex":
		claims := map[string]any{"exp": int64(4102444800), "sub": identity, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": identity, "chatgpt_user_id": identity}}
		token := "x." + base64.RawURLEncoding.EncodeToString(canonicalFixtureJSON(t, claims)) + ".inert-signature"
		return map[string][]byte{"auth.json": canonicalFixtureJSON(t, map[string]any{"auth_mode": "chatgpt", "last_refresh": "2026-01-01T00:00:00Z", "tokens": map[string]any{"id_token": token, "access_token": token, "refresh_token": refresh, "account_id": identity}})}
	case "gemini":
		return map[string][]byte{
			"settings.json":        []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`),
			"oauth_creds.json":     canonicalFixtureJSON(t, map[string]any{"access_token": access, "refresh_token": refresh, "expiry_date": int64(4102444800000), "token_type": "Bearer"}),
			"google_accounts.json": canonicalFixtureJSON(t, map[string]any{"active": identity + "@example.invalid", "old": []string{}}),
		}
	case "grok":
		return map[string][]byte{"auth.json": canonicalFixtureJSON(t, map[string]any{"https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828": map[string]any{"key": access, "refresh_token": refresh, "auth_mode": "oauth", "oidc_issuer": "https://auth.x.ai", "oidc_client_id": "b1a00492-073a-47ea-816f-4c329264a828", "principal_type": "user", "principal_id": identity, "user_id": identity, "team_id": "fixture-team", "create_time": "2026-01-01T00:00:00Z", "expires_at": "2100-01-01T00:00:00Z"}})}
	default:
		t.Fatalf("unknown canonical fixture provider %q", provider)
		return nil
	}
}
