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
	existing := writeTmp(t, "config.toml",
		"model = \"o3\"\n\n[mcp_servers.stale]\ncommand = \"gone\"\n\n[mcp_servers_backup]\nnote = \"keep me\"\n")
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
	// A lookalike table whose name merely starts with "mcp_servers" is NOT an MCP table and must survive.
	if !strings.Contains(got, "[mcp_servers_backup]") || !strings.Contains(got, "keep me") {
		t.Errorf("a [mcp_servers_backup] table must be preserved (only real [mcp_servers.*] tables are stripped):\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.ctx7]") {
		t.Error("shared servers should be present")
	}
}

// A numeric MCP env value (JSON numbers decode to float64) must render as plain digits, never
// scientific notation — "8080", not "8.08e+03"; a big value not "1.23e+19".
func TestGenerateCodexNumericEnvNoScientificNotation(t *testing.T) {
	mcp := `{"mcpServers":{"s":{"command":"x","env":{"PORT":8080,"BIG":12345678901234567890}}}}`
	got, err := GenerateCodex(writeTmp(t, "mcp.json", mcp), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `PORT = "8080"`) {
		t.Errorf(`PORT should render as "8080":\n%s`, got)
	}
	if strings.Contains(got, "e+") || strings.Contains(got, "E+") {
		t.Errorf("numeric env must not render in scientific notation:\n%s", got)
	}
}

// A server name with a dot/space, and an env value with a control char, must produce VALID TOML:
// the name is quoted (else a dot nests the table / a space breaks the parse) and \n is escaped.
func TestGenerateCodexQuotesNonBareNamesAndEscapes(t *testing.T) {
	mcp := `{"mcpServers":{"my.server":{"command":"x","env":{"K":"a\nb"}}}}`
	got, err := GenerateCodex(writeTmp(t, "mcp.json", mcp), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `[mcp_servers."my.server"]`) {
		t.Errorf("a dotted server name must be quoted (else it nests the table):\n%s", got)
	}
	if strings.Contains(got, "a\nb") || !strings.Contains(got, `K = "a\nb"`) {
		t.Errorf("a control char in an env value must be escaped (no raw newline):\n%s", got)
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

// A present-but-malformed existing gemini settings file must error (so box.Run skips wiring and
// gemini keeps its real config), not silently produce a settings.json containing only mcpServers.
func TestGenerateGeminiMalformedExistingErrors(t *testing.T) {
	mcpFile := writeTmp(t, "mcp.json", sample)
	if _, err := GenerateGemini(mcpFile, writeTmp(t, "settings.json", `{"theme":"dark", oops`)); err == nil {
		t.Error("malformed existing settings.json should error, not discard the user's settings")
	}
	// Missing/empty existing is fine — nothing to merge onto.
	if _, err := GenerateGemini(mcpFile, filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Errorf("missing existing settings should be ok, got %v", err)
	}
	if _, err := GenerateGemini(mcpFile, writeTmp(t, "empty.json", "  \n")); err != nil {
		t.Errorf("empty existing settings should be ok, got %v", err)
	}
}

// A server with neither command nor url is skipped, not emitted as a bodyless table that would
// break Codex's whole config parse.
func TestGenerateCodexSkipsTransportlessServer(t *testing.T) {
	got, err := GenerateCodex(writeTmp(t, "mcp.json", `{"mcpServers":{"good":{"command":"x"},"broken":{}}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "broken") {
		t.Errorf("transport-less server should be skipped, got:\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.good]") {
		t.Errorf("valid server should remain, got:\n%s", got)
	}
}

// A canonical HTTP/SSE server ({type, url, headers}) passes through to gemini's settings.json
// verbatim — gemini honors that exact shape (verified against `gemini mcp add -t http/-t sse`), so
// the raw passthrough is gemini-native. Locked in so a future GenerateGemini refactor can't break it.
func TestGenerateGeminiHTTPPassthrough(t *testing.T) {
	src := `{ "mcpServers": {
		"emisar": { "type": "http", "url": "https://emisar.dev/api/mcp/rpc", "headers": { "Authorization": "Bearer emk-x" } },
		"legacy": { "type": "sse",  "url": "https://legacy.example/sse",      "headers": { "X-Api-Key": "k" } }
	} }`
	got, err := GenerateGemini(writeTmp(t, "mcp.json", src), "")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(got), &f); err != nil {
		t.Fatalf("gemini settings not valid JSON: %v\n%s", err, got)
	}
	em := f.MCPServers["emisar"]
	if em["type"] != "http" || em["url"] != "https://emisar.dev/api/mcp/rpc" {
		t.Errorf("emisar type/url not preserved for gemini: %+v", em)
	}
	if h, ok := em["headers"].(map[string]any); !ok || h["Authorization"] != "Bearer emk-x" {
		t.Errorf("emisar headers not preserved for gemini: %+v", em["headers"])
	}
	if f.MCPServers["legacy"]["type"] != "sse" {
		t.Errorf("sse type not preserved for gemini: %+v", f.MCPServers["legacy"])
	}
}

// Codex has no inline-header support, so a url server with headers but no bearer_token_env_var would
// authenticate nowhere — GenerateCodex flags it rather than emit a silent unauthenticated url. A
// bearer_token_env_var (codex's real mechanism) emits cleanly, with no notice.
func TestGenerateCodexHTTPHeaders(t *testing.T) {
	src := `{ "mcpServers": {
		"hdronly": { "type": "http", "url": "https://a.example/mcp", "headers": { "Authorization": "Bearer x" } },
		"bearer":  { "type": "http", "url": "https://b.example/mcp", "bearer_token_env_var": "B_TOKEN" }
	} }`
	got, err := GenerateCodex(writeTmp(t, "mcp.json", src), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `bearer_token_env_var = "B_TOKEN"`) {
		t.Errorf("expected bearer_token_env_var for the bearer server:\n%s", got)
	}
	if n := strings.Count(got, "# coop: codex can't use"); n != 1 {
		t.Errorf("the header-gap notice should fire exactly once (only the headers-only server), got %d:\n%s", n, got)
	}
}
