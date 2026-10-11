package agent

import "testing"

func TestClaudeBrokerOrganizationPolicyLimitsRoute(t *testing.T) {
	for _, test := range []struct{ selection, header, prefix string }{
		{"claude-oauth", "Authorization", "Bearer "}, {"apikey", "X-Api-Key", ""},
	} {
		t.Run(test.selection, func(t *testing.T) {
			routes, err := claudeBrokerRoutes(test.selection)
			if err != nil {
				t.Fatal(err)
			}
			found := 0
			for _, route := range routes {
				if route.Path != "/api/claude_code/policy_limits" {
					continue
				}
				found++
				if route.Host != "api.anthropic.com" || route.Method != "GET" || route.Query != "" || route.Segment != "" || len(route.Queries) != 0 || route.CredentialFree || route.Header != test.header || route.HeaderPrefix != test.prefix || route.AccountHeader != "" {
					t.Fatalf("organization policy route widened or changed authentication: %#v", route)
				}
			}
			if found != 1 {
				t.Fatalf("organization policy route count = %d, want 1", found)
			}
		})
	}
}
