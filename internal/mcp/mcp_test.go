package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTmp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const sample = `{
  "mcpServers": {
    "ctx7":   { "command": "npx", "args": ["-y", "@upstash/context7-mcp"], "env": { "K": "v" } },
    "sentry": { "type": "http", "url": "https://mcp.sentry.dev/mcp" }
  }
}`

func TestGenerateCodex(t *testing.T) {
	got, err := GenerateCodex(writeTmp(t, "mcp.json", sample), "")
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic, sorted-by-name output — lock the exact format.
	want := `[mcp_servers.ctx7]
command = "npx"
args = ["-y", "@upstash/context7-mcp"]

[mcp_servers.ctx7.env]
K = "v"

[mcp_servers.sentry]
url = "https://mcp.sentry.dev/mcp"
`
	if got != want {
		t.Errorf("GenerateCodex mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestGenerateCodexPreservesExistingConfig(t *testing.T) {
	existing := writeTmp(t, "config.toml", "model = \"o3\"\n\n[mcp_servers.stale]\ncommand = \"gone\"\n")
	got, err := GenerateCodex(writeTmp(t, "mcp.json", sample), existing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `model = "o3"`) {
		t.Error("non-mcp config should be preserved")
	}
	if strings.Contains(got, "stale") {
		t.Error("the user's own [mcp_servers.*] should be replaced, not kept")
	}
	if !strings.Contains(got, "[mcp_servers.ctx7]") {
		t.Error("shared servers should be present")
	}
}

func TestGenerateGeminiMerge(t *testing.T) {
	existing := writeTmp(t, "settings.json", `{"theme":"dark","mcpServers":{"old":{"command":"true"}}}`)
	got, err := GenerateGemini(writeTmp(t, "mcp.json", sample), existing)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Theme      string                    `json:"theme"`
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got)
	}
	if out.Theme != "dark" {
		t.Error("existing top-level setting (theme) must be preserved")
	}
	for _, name := range []string{"old", "ctx7", "sentry"} {
		if _, ok := out.MCPServers[name]; !ok {
			t.Errorf("server %q missing from merged settings", name)
		}
	}
}

func TestGenerateMalformed(t *testing.T) {
	if _, err := GenerateCodex(writeTmp(t, "mcp.json", "{not json"), ""); err == nil {
		t.Error("malformed mcp.json should error")
	}
	if _, err := GenerateGemini(filepath.Join(t.TempDir(), "missing.json"), ""); err == nil {
		t.Error("missing mcp.json should error")
	}
}

func TestGenerateEmpty(t *testing.T) {
	got, err := GenerateCodex(writeTmp(t, "mcp.json", `{"mcpServers":{}}`), "")
	if err != nil || got != "" {
		t.Errorf("empty servers -> empty codex output; got %q err %v", got, err)
	}
}
