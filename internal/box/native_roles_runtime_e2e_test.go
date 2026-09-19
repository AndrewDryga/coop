//go:build boxruntimee2e

package box

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// Each pinned client finds the native role Coop renders for it: the definition lands where that
// client loads agents from, and the client's own offline check, inside the locked client image with
// networking off, reads it back. A strict parser drops a definition without a word, so this is the
// proof a lead that delegates to coop-<role> will find it. Needs the locked client image, which
// `coop net setup` builds; run it with `make native-roles-e2e`.
func TestRuntimeNativeRolesAreDiscoveredByEveryPinnedClient(t *testing.T) {
	rt, err := runtime.Detect(os.Getenv("COOP_RUNTIME"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	docker, err := runtime.InspectDocker(ctx, rt)
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	binding := networkRuntimeBinding(docker.Info(), docker.Endpoint())
	definition, _, _, err := lockedImageDefinition(agents.ClientPlatform{OS: binding.OS, Architecture: binding.Architecture, Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := docker.Image(ctx, definition.Tag); err != nil {
		t.Fatalf("the locked client image %s is not on this daemon; run `coop net setup` first: %v", definition.Tag, err)
	}
	for _, test := range []struct {
		provider, model, check string
		canaries               map[string]string // siblings the client must reject by name, proving it validates that directory
		found                  func(output string) bool
	}{
		{"claude", "claude-opus-4-8", `claude plugin validate "$HOME/.claude/agents"`, nil, func(out string) bool {
			return strings.Contains(out, "Validation passed") && !strings.Contains(out, "✘")
		}},
		// Codex lists nothing it loaded, only what it refused: a syntax canary and a schema canary (valid
		// TOML, an unknown field) must both be named, and the rendered role never.
		{"codex", "gpt-5.6-sol", `CODEX_HOME="$HOME/.codex" codex doctor`,
			map[string]string{"coop-syntax.toml": "not = = a role\n", "coop-schema.toml": "name = \"coop-schema\"\ndeveloper_instructions = \"x\"\nnot_a_field = 1\n"},
			func(out string) bool {
				return strings.Contains(out, "coop-syntax.toml") && strings.Contains(out, "unknown field `not_a_field`") &&
					!strings.Contains(out, "coop-thinker.toml")
			}},
		// Gemini reports how many agents it loaded: the count without the rendered file, then with it,
		// must differ by exactly that one agent.
		{"gemini", "gemini-3.5-flash", `export GEMINI_API_KEY=unused GEMINI_CLI_TRUST_WORKSPACE=true; ` +
			`mv "$HOME/.gemini/agents/coop-thinker.md" /tmp/held.md && timeout 20 gemini --debug -p ping | grep -o "Loaded with [0-9]* agents"; ` +
			`mv /tmp/held.md "$HOME/.gemini/agents/coop-thinker.md" && timeout 20 gemini --debug -p ping | grep -E "Loaded with [0-9]* agents|Error loading user agent"`,
			nil, func(out string) bool {
				counts := regexp.MustCompile(`Loaded with (\d+) agents`).FindAllStringSubmatch(out, -1)
				if len(counts) != 2 || strings.Contains(out, "Error loading user agent") {
					return false
				}
				before, _ := strconv.Atoi(counts[0][1])
				after, _ := strconv.Atoi(counts[1][1])
				return after == before+1
			}},
		{"grok", "grok-4.5", `grok inspect --json`, nil, func(out string) bool {
			return strings.Contains(out, `"name": "coop-thinker"`)
		}},
	} {
		t.Run(test.provider, func(t *testing.T) {
			ag, _ := agents.Get(test.provider)
			support := ag.NativeSubagents()
			role := agents.NativeSubagent{Name: "coop-thinker", Description: "Use for: architecture, code-review.", Model: test.model,
				Prompt: "You are the thinker: say \"why\" first.\n\n- then: the plan"}
			if support.Effort != nil {
				role.Effort = "high"
			}
			source := t.TempDir()
			dir := filepath.Join(source, filepath.FromSlash(support.HomeDir))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			file, content := support.Render(role)
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			for name, body := range test.canaries {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			script := `mkdir -p /tmp/h && cp -R /src/. /tmp/h/ && cd /tmp/h && ` + test.check + ` 2>&1`
			out, _ := exec.CommandContext(ctx, rt.Name, "run", "--rm", "--network", "none", "-e", "HOME=/tmp/h",
				"-v", source+":/src:ro", "--entrypoint", "sh", definition.Tag, "-c", script).CombinedOutput()
			if !test.found(string(out)) {
				t.Fatalf("%s did not load the rendered native role:\n%s\n--- rendered %s ---\n%s", test.provider, out, file, content)
			}
		})
	}
}
