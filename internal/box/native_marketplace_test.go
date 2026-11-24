package box

import (
	"slices"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

func TestNativeRunIncludesPublicDownloadOriginOnlyWhenFiltered(t *testing.T) {
	ag, _ := agents.Get("claude")
	routes, err := ag.NativeCredentials().Broker.Routes("claude-oauth")
	if err != nil {
		t.Fatal(err)
	}
	filtered := &nativeRun{
		filtered: true,
		accounts: []*nativeRunAccount{{agent: ag, routes: routes}},
	}
	if !slices.Equal(filtered.origins(), []string{"api.anthropic.com", "downloads.claude.ai", "mcp-proxy.anthropic.com"}) {
		t.Fatalf("filtered Claude native origins = %v", filtered.origins())
	}
	open := &nativeRun{
		filtered: false,
		accounts: []*nativeRunAccount{{agent: ag, routes: routes}},
	}
	if !slices.Equal(open.origins(), []string{"api.anthropic.com", "mcp-proxy.anthropic.com"}) {
		t.Fatalf("open Claude native origins = %v, want public download origin omitted", open.origins())
	}
}
