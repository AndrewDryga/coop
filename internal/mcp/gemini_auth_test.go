package mcp

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestGenerateGeminiUsesNativeBearerReferences(t *testing.T) {
	t.Setenv("MCP_TOKEN", "must-not-persist-this-token")
	for _, transport := range []string{"http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			body := `{"mcpServers":{"emisar":{"type":"` + transport + `","url":"https://emisar.example/mcp","headers":{"X-Tenant":"test"},"bearer_token_env_var":"MCP_TOKEN"},"other":{"url":"https://other.example/mcp","bearer_token_env_var":"MCP_TOKEN"}}}`
			file := writeTmp(t, "mcp.json", body)
			got, required, err := GenerateGemini(file, "")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(required, []string{"MCP_TOKEN"}) || strings.Contains(got, "bearer_token_env_var") || strings.Contains(got, "must-not-persist") {
				t.Fatalf("native projection leaked a token, retained a foreign field or lost required environment: %q", required)
			}
			var result struct{ MCPServers map[string]server }
			if err := json.Unmarshal([]byte(got), &result); err != nil {
				t.Fatal(err)
			}
			s := result.MCPServers["emisar"]
			if s.Type != transport || s.URL != "https://emisar.example/mcp" || s.Headers["X-Tenant"] != "test" || s.Headers["Authorization"] != "Bearer ${MCP_TOKEN}" {
				t.Fatalf("native projection lost transport or authentication: %+v", s)
			}
			if after, err := os.ReadFile(file); err != nil || string(after) != body {
				t.Fatal("projection changed the shared source")
			}
		})
	}
}

func TestGenerateGeminiRefusesInvalidBearerReferences(t *testing.T) {
	for _, ref := range []any{"", " ", "1TOKEN", "TOKEN; touch file", "${TOKEN}", true, nil} {
		definition := map[string]any{"url": "https://example.test/mcp", "bearer_token_env_var": ref}
		data, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"test": definition}})
		if _, _, err := GenerateGemini(writeTmp(t, "mcp.json", string(data)), ""); err == nil {
			t.Fatalf("accepted invalid reference %v", ref)
		}
	}
	for _, definition := range []string{
		`{"url":"https://example.test/mcp","bearer_token_env_var":"TOKEN","headers":{"authorization":"competing"}}`,
		`{"command":"tool","bearer_token_env_var":"TOKEN"}`,
		`{"url":"","bearer_token_env_var":"TOKEN"}`,
	} {
		if _, _, err := GenerateGemini(writeTmp(t, "mcp.json", `{"mcpServers":{"test":`+definition+`}}`), ""); err == nil {
			t.Fatal("accepted ambiguous or non-HTTP authentication")
		}
	}
}
