package agent

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func TestLockedClientsAreCompletePinnedAndFresh(t *testing.T) {
	var previous string
	for _, arch := range []string{"amd64", "arm64"} {
		p := ClientPlatform{"linux", arch, "glibc"}
		closure, err := LockedClientClosure(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(closure.Clients) != 8 || len(closure.Files) != 8 || len(closure.Digest) != 64 || closure.Digest == previous {
			t.Fatal("incomplete or platform-ambiguous closure", closure.Digest)
		}
		previous = closure.Digest
		for _, client := range closure.Clients {
			script := string(closure.Files["launchers/"+client.Binary])
			if !strings.Contains(script, "unset NODE_OPTIONS NODE_PATH") || !strings.Contains(script, "exec '") || !strings.HasSuffix(script, " \"$@\"\n") {
				t.Fatal("uncontrolled launcher", script)
			}
			cmd := exec.Command("sh", "-n")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatal(string(out), err)
			}
			for _, key := range client.UnsetEnv {
				if !strings.Contains(script, " "+key+"\n") {
					t.Fatal("override survives", key)
				}
			}
		}
		want := closure.Digest
		closure.Files["package.json"][0] = '!'
		closure.Clients[0].Exec[0] = "/repo/shim"
		again, err := LockedClientClosure(p)
		if err != nil || again.Digest != want {
			t.Fatal("caller mutated shared closure", err)
		}
	}
	for _, p := range []ClientPlatform{{"darwin", "arm64", "glibc"}, {"linux", "arm64", "musl"}, {"linux", "386", "glibc"}, {}} {
		if _, err := LockedClientClosure(p); err == nil {
			t.Fatal("unsupported platform", p)
		}
		for _, name := range Names() {
			if a, _ := Get(name); len(a.LockedClients(p)) != 0 {
				t.Fatal("adapter declared unsupported platform", name, p)
			}
		}
	}
}

func TestLockedClosureRejectsMutableOrInconsistentInputs(t *testing.T) {
	for _, change := range []string{"floating-root", "lock-root-drift", "missing-root", "missing-integrity", "untrusted-registry", "git-url", "link", "bad-path", "bad-exec", "bad-env", "duplicate-binary", "duplicate-variant", "conflicting-shared-binary", "conflicting-shared-package", "bad-native-digest", "bad-native-url", "wrong-native-platform", "clobber-node", "missing-native-lock"} {
		t.Run(change, func(t *testing.T) {
			c, err := LockedClientClosure(ClientPlatform{"linux", "arm64", "glibc"})
			if err != nil {
				t.Fatal(err)
			}
			var lock map[string]any
			if err := json.Unmarshal(c.Files["package-lock.json"], &lock); err != nil {
				t.Fatal(err)
			}
			packages := lock["packages"].(map[string]any)
			name := "node_modules/" + c.Clients[0].Package
			item := packages[name].(map[string]any)
			switch change {
			case "floating-root":
				c.Files["package.json"] = bytes.ReplaceAll(c.Files["package.json"], []byte(c.Clients[0].Version), []byte("latest"))
			case "lock-root-drift":
				item["version"] = "999.0.0"
			case "missing-root":
				delete(packages, name)
			case "missing-integrity":
				delete(item, "integrity")
			case "untrusted-registry":
				item["resolved"] = "https://unreviewed.invalid/package.tgz"
			case "git-url":
				item["resolved"] = "git+https://registry.npmjs.org/package.tgz"
			case "link":
				item["link"] = true
			case "bad-path":
				packages["node_modules/../../escape"] = item
			case "bad-exec":
				c.Clients[0].Exec = []string{"/workspace/claude"}
			case "bad-env":
				c.Clients[0].UnsetEnv = []string{"KEY; exit 0"}
			case "duplicate-binary":
				c.Clients[1].Binary = c.Clients[0].Binary
			case "duplicate-variant":
				c.Clients[1].Client = c.Clients[0].Client
			case "conflicting-shared-binary":
				c.Clients[len(c.Clients)-1].Exec = []string{"/opt/coop/clients/native/other"}
			case "conflicting-shared-package":
				c.Clients[len(c.Clients)-4].Binary = "other-gemini"
			case "bad-native-digest":
				c.Clients[len(c.Clients)-1].NativeArtifact.SHA256 = strings.Repeat("0", 63)
			case "bad-native-url":
				c.Clients[len(c.Clients)-1].NativeArtifact.URL = "https://example.com/grok.gz"
			case "wrong-native-platform":
				c.Clients[len(c.Clients)-1].NativeArtifact.URL = strings.ReplaceAll(c.Clients[len(c.Clients)-1].NativeArtifact.URL, "aarch64", "x86_64")
			case "clobber-node":
				c.Clients[0].Binary = "node"
			case "missing-native-lock":
				delete(packages, "node_modules/@anthropic-ai/claude-code-linux-arm64")
			}
			c.Files["package-lock.json"], err = json.Marshal(lock)
			if err != nil {
				t.Fatal(err)
			}
			if validateClientClosure(c.Platform, c.Files, c.Clients) == nil {
				t.Fatal("accepted mutable/inconsistent closure")
			}
		})
	}
}

func TestLockedClaudeNativeAndAdapterExecutablesStayDistinct(t *testing.T) {
	p := ClientPlatform{"linux", "amd64", "glibc"}
	claude := claudeAgent{}.LockedClients(p)
	if !slices.Equal(claude[0].Exec, []string{"/opt/coop/clients/node_modules/@anthropic-ai/claude-code-linux-x64/claude"}) {
		t.Fatal("native CLI regressed to sync Node wrapper", claude[0].Exec)
	}
	if !slices.Contains(claude[1].UnsetEnv, "CLAUDE_CODE_EXECUTABLE") || !strings.HasSuffix(claude[1].Exec[1], "/claude-agent-acp/dist/index.js") {
		t.Fatal("ACP silently changed SDK/native client", claude[1])
	}
	codex := codexAgent{}.LockedClients(p)
	if !slices.Contains(codex[1].UnsetEnv, "CODEX_PATH") {
		t.Fatal("ACP executable override retained")
	}
}

// TestProviderBundlesCarryFunctionNotChatter is the check of the provider-bundle rule: a core
// bundle holds what the client needs to FUNCTION on the selected login — the API, the token
// endpoint, claude.ai's connector proxy — and never the client's own release feed, package
// registry or telemetry intake. Those are switched off in the box (BoxEnv, the generated
// overlays); a later request to one is a real refusal, recorded and explainable, never granted
// in advance and never hidden. The exact set is pinned beside its version: a changed set ships
// under a new NetworkBundleVersion or not at all.
func TestProviderBundlesCarryFunctionNotChatter(t *testing.T) {
	chatter := []string{
		"raw.githubusercontent.com", "objects.githubusercontent.com", "api.github.com", // release feeds, announcements
		"registry.npmjs.org", "downloads.claude.ai", "storage.googleapis.com", "formulae.brew.sh", // installs and updates
		"datadoghq.com", "datadoghq.eu", "sentry.io", "ab.chatgpt.com", "statsig.com", // telemetry and error intakes
	}
	for _, name := range Names() {
		ag, _ := Get(name)
		for _, client := range []egress.Client{egress.ClientCLI, egress.ClientACP} {
			bundle, err := ag.NetworkBundle(NetworkBundleInput{Client: client})
			if err != nil {
				continue
			}
			for _, rule := range bundle.Core {
				for _, host := range chatter {
					if rule.To.Domain == host || strings.HasSuffix(rule.To.Domain, "."+host) {
						t.Errorf("%s core bundle grants %s: switch the client's chatter off in the box instead of widening egress", name, rule.To.Domain)
					}
				}
			}
			for feature, rules := range bundle.Features {
				for _, rule := range rules {
					for _, host := range chatter {
						if rule.To.Domain == host || strings.HasSuffix(rule.To.Domain, "."+host) {
							t.Errorf("%s feature %s grants %s", name, feature, rule.To.Domain)
						}
					}
				}
			}
		}
	}
	claude, err := claudeAgent{}.NetworkBundle(NetworkBundleInput{Client: egress.ClientCLI})
	if err != nil {
		t.Fatal(err)
	}
	var domains []string
	for _, rule := range claude.Core {
		domains = append(domains, rule.To.Domain)
	}
	if want := []string{"api.anthropic.com", "platform.claude.com", "mcp-proxy.anthropic.com"}; !slices.Equal(domains, want) || claude.Version != NetworkBundleVersion {
		t.Fatalf("claude core = %v under %s; want %v under %s (a different set needs its own NetworkBundleVersion)", domains, claude.Version, want, NetworkBundleVersion)
	}
}

func TestNetworkBundleAndLockedClientSupportAgree(t *testing.T) {
	for _, name := range Names() {
		ag, _ := Get(name)
		supported := len(ag.LockedClients(ClientPlatform{"linux", "arm64", "glibc"})) != 0
		for _, client := range []egress.Client{egress.ClientCLI, egress.ClientACP} {
			bundle, err := ag.NetworkBundle(NetworkBundleInput{Client: client})
			if supported != (err == nil) {
				t.Fatalf("%s: locked client and endpoint bundle support disagree for %s: %v", name, client, err)
			}
			if !supported {
				continue
			}
			if bundle.Provider != name || bundle.Client != client || bundle.Backend != "direct" ||
				bundle.AuthMode == "" || bundle.Version != NetworkBundleVersion || len(bundle.Core) == 0 || len(bundle.Sources) == 0 {
				t.Fatalf("%s: bundle is not an identified release-owned selection: %+v", name, bundle)
			}
			for _, rule := range bundle.Core {
				if rule.To.Domain == "" || rule.Protocol != "tls" || !slices.Equal(rule.Ports, []int{443}) {
					t.Fatalf("%s: core endpoint is outside the qualified TLS443 subset: %+v", name, rule)
				}
			}
			// Selection is the operator's; an override the adapter cannot serve
			// must refuse instead of silently returning the default tuple.
			for _, override := range []NetworkBundleInput{{Client: client, Backend: "gateway"}, {Client: client, AuthMode: "unsupported"}, {Client: "console"}} {
				if _, err := ag.NetworkBundle(override); err == nil {
					t.Fatalf("%s: unqualified selection %+v was served", name, override)
				}
			}
		}
	}
}
