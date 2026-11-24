package agent

import (
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func TestClaudeBrokerMarketplaceRoutes(t *testing.T) {
	for _, selection := range []string{"claude-oauth", "apikey"} {
		t.Run(selection, func(t *testing.T) {
			routes, err := claudeBrokerRoutes(selection)
			if err != nil {
				t.Fatal(err)
			}
			var cdn []NativeBrokerRoute
			for _, route := range routes {
				if route.Host == "github.com" {
					t.Fatalf("claude broker granted github.com route: %#v", route)
				}
				if route.Host == "downloads.claude.ai" {
					cdn = append(cdn, route)
				}
			}
			if len(cdn) != 2 {
				t.Fatalf("downloads.claude.ai routes = %d, want 2: %#v", len(cdn), cdn)
			}
			latest, archive := cdn[0], cdn[1]
			if !latest.CredentialFree || !latest.RejectCredentials || latest.Method != "GET" ||
				latest.Path != "/claude-code-releases/plugins/claude-plugins-official/latest" ||
				latest.Query != "" || len(latest.Queries) != 0 || latest.Segment != "" || latest.Suffix != "" ||
				latest.Header != "" || latest.HeaderPrefix != "" || latest.AccountHeader != "" ||
				latest.MaxResponseBytes != 256 || latest.SetsPointer != "claude-plugins-official" || latest.UsesPointer != "" {
				t.Fatalf("unexpected marketplace latest route: %#v", latest)
			}
			if !archive.CredentialFree || !archive.RejectCredentials || archive.Method != "GET" ||
				archive.Path != "/claude-code-releases/plugins/claude-plugins-official/" ||
				archive.Query != "" || len(archive.Queries) != 0 || archive.Segment != "sha1" || archive.Suffix != ".zip" ||
				archive.Header != "" || archive.HeaderPrefix != "" || archive.AccountHeader != "" ||
				archive.MaxResponseBytes != 64<<20 || archive.SetsPointer != "" || archive.UsesPointer != "claude-plugins-official" {
				t.Fatalf("unexpected marketplace archive route: %#v", archive)
			}
		})
	}
	ag, _ := Get("claude")
	for _, client := range []egress.Client{egress.ClientCLI, egress.ClientACP} {
		bundle, err := ag.NetworkBundle(NetworkBundleInput{Client: client})
		if err != nil {
			t.Fatal(err)
		}
		for _, rule := range bundle.Core {
			if rule.To.Domain == "downloads.claude.ai" || rule.To.Domain == "github.com" {
				t.Fatalf("claude %s core bundle widened to %s", client, rule.To.Domain)
			}
		}
	}
	env := ag.BoxEnv("/home/node")
	for _, want := range []string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_UPDATES=1"} {
		if !slices.Contains(env, want) {
			t.Fatalf("claude BoxEnv missing %s: %v", want, env)
		}
	}
	for _, entry := range env {
		if entry == "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1" {
			t.Fatalf("claude BoxEnv disabled official marketplace registration: %v", env)
		}
	}
}
