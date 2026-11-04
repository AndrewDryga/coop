package agent

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestCodexBrokerDeclaresOnlyAccountDiscoveryProjection(t *testing.T) {
	for _, selection := range []string{"chatgpt", "apikey"} {
		routes, err := codexBrokerRoutes(selection)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, route := range routes {
			if route.AccountResponse == nil {
				continue
			}
			found++
			if selection != "chatgpt" || route.Host != "chatgpt.com" || route.Method != "GET" || route.Path != "/backend-api/wham/accounts/check" || route.Query != "" || len(route.Queries) != 0 || route.Segment != "" || route.CredentialFree || route.AccountHeader != route.AccountResponse.Header {
				t.Fatalf("account projection widened: %#v", route)
			}
			if *route.AccountResponse != (NativeBrokerAccountResponse{Header: "Chatgpt-Account-Id", CollectionKey: "accounts", IDKey: "id", LegacyObjectKey: "account", LegacyIDKey: "account_id", OrderingKey: "account_ordering", DefaultIDKey: "default_account_id"}) {
				t.Fatal("native discovery grammar changed")
			}
		}
		want := 0
		if selection == "chatgpt" {
			want = 1
		}
		if found != want {
			t.Fatalf("%s projection count=%d, want %d", selection, found, want)
		}
	}
}

func TestNativeBrokerPublicSeedsAndDivergence(t *testing.T) {
	for _, test := range []struct{ provider, selection string }{
		{"claude", "claude-oauth"}, {"codex", "chatgpt"}, {"codex", "apikey"},
		{"gemini", "oauth-personal"}, {"gemini", "gemini-api-key"}, {"grok", grokNativeScope},
	} {
		t.Run(test.provider+"/"+test.selection, func(t *testing.T) {
			ag, _ := Get(test.provider)
			spec := ag.NativeCredentials()
			seed, err := spec.Broker.Seed(test.selection)
			if err != nil {
				t.Fatal(err)
			}
			again, err := spec.Broker.Seed(test.selection)
			if err != nil {
				t.Fatal(err)
			}
			first, _ := json.Marshal(seed)
			second, _ := json.Marshal(again)
			if !bytes.Equal(first, second) || seed.Marker == "" {
				t.Fatal("public seed depends on launch/account state")
			}
			if err := spec.Broker.Check(seed.Files, seed); err != nil {
				t.Fatal(err)
			}
			routes, err := spec.Broker.Routes(test.selection)
			if err != nil || len(routes) == 0 {
				t.Fatal("missing original native routes", err)
			}
			for _, route := range routes {
				if route.Host == "" || route.Method == "" || route.Path == "" {
					t.Fatal("incomplete native route")
				}
				if route.CredentialFree {
					if route.Header != "" || route.HeaderPrefix != "" || route.AccountHeader != "" {
						t.Fatal("public download carries account authority")
					}
				} else if route.Header == "" {
					t.Fatal("authenticated route has no credential header")
				}
			}
			files := cloneNativeFiles(seed.Files)
			switch test.provider {
			case "claude":
				files[".credentials.json"] = []byte(`{"claudeAiOauth":{"accessToken":"unexpected-local-grant"}}`)
			case "codex":
				var auth map[string]any
				if err := json.Unmarshal(files["auth.json"], &auth); err != nil {
					t.Fatal(err)
				}
				auth["tokens"].(map[string]any)["access_token"] = "unexpected-local-grant"
				files["auth.json"], err = json.Marshal(auth)
				if err != nil {
					t.Fatal(err)
				}
			case "gemini":
				files["oauth_creds.json"] = []byte(`{"access_token":"unexpected-local-grant"}`)
			case "grok":
				files["auth.json"] = []byte(`{"` + grokNativeScope + `":{"key":"unexpected-local-grant","auth_mode":"external","oidc_issuer":"` + grokIssuer + `"}}`)
			}
			before, _ := json.Marshal(files)
			if err := spec.Broker.Check(files, seed); err == nil {
				t.Fatal("local credential divergence accepted")
			}
			after, _ := json.Marshal(files)
			if !bytes.Equal(before, after) {
				t.Fatal("divergence check discarded local credentials")
			}
		})
	}
}

// Grok reads this record instead of asking the helper, so a new home cannot start without it. Measured
// on grok 1.0.44: a record with no user_id reads as not signed in, and an expired one is renewed through
// the helper on its first use.
func TestGrokSeedLetsANewHomeStart(t *testing.T) {
	seed, err := seedGrokBroker(grokNativeScope)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]string
	if err := json.Unmarshal(seed.Files["auth.json"], &doc); err != nil {
		t.Fatal(err)
	}
	record := doc[grokNativeScope]
	for _, field := range []string{"user_id", "email", "create_time", "expires_at"} {
		if record[field] == "" {
			t.Errorf("the record has no %s: %s", field, seed.Files["auth.json"])
		}
	}
	if record["key"] != seed.Marker || record["auth_mode"] != "external" || record["oidc_issuer"] != grokIssuer {
		t.Errorf("the record does not name the broker's helper: %s", seed.Files["auth.json"])
	}
	if expires, err := time.Parse(time.RFC3339, record["expires_at"]); err != nil || expires.After(time.Now()) {
		t.Errorf("the record must have expired, so its first use asks the helper: %q, %v", record["expires_at"], err)
	}
	for _, grant := range []string{"refresh_token", "id_token"} {
		if _, held := record[grant]; held {
			t.Errorf("the public record holds %s: %s", grant, seed.Files["auth.json"])
		}
	}
}
