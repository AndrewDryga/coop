package agent

import (
	"bytes"
	"encoding/json"
	"testing"
)

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
