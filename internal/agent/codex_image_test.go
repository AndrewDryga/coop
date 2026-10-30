package agent

import "testing"

func TestCodexImageRoutesKeepSelectedAccountAndExactEndpoints(t *testing.T) {
	// A production cat request reached the image tool, but our missing route
	// returned broker403 and Ryker delivered ASCII art instead of an image.
	// Codex rust-v0.159.2: codex-api/src/endpoint/images.rs uses both suffixes
	// through the selected provider, including its API-key family.
	for _, family := range []struct{ selection, host, base, account string }{
		{"chatgpt", "chatgpt.com", "/backend-api/codex", "Chatgpt-Account-Id"},
		{"apikey", "api.openai.com", "/v1", ""},
	} {
		for _, operation := range []string{"generations", "edits"} {
			t.Run(family.selection+"/"+operation, func(t *testing.T) {
				routes, err := codexBrokerRoutes(family.selection)
				if err != nil {
					t.Fatal(err)
				}
				want := NativeBrokerRoute{Host: family.host, Method: "POST", Path: family.base + "/images/" + operation,
					Header: "Authorization", HeaderPrefix: "Bearer ", AccountHeader: family.account}
				found := 0
				for _, route := range routes {
					if route.Path == want.Path {
						found++
						if route.Host != want.Host || route.Method != want.Method || route.Header != want.Header || route.HeaderPrefix != want.HeaderPrefix || route.AccountHeader != want.AccountHeader || route.CredentialFree || route.Query != "" || len(route.Queries) != 0 || route.Segment != "" || route.Suffix != "" || route.AccountResponse != nil {
							t.Fatalf("image route widened or changed authentication: %#v", route)
						}
					}
				}
				if found != 1 {
					t.Fatalf("image route count = %d, want 1", found)
				}
			})
		}
	}
}
