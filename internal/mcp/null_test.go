package mcp

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestMCPNullServerDefinitionIsRejected(t *testing.T) {
	path := writeTmp(t, "mcp.json", `{"mcpServers":{"broken":null}}`)
	for name, run := range map[string]func() error{
		"Codex":    func() error { _, _, err := GenerateCodex(path, ""); return err },
		"Grok":     func() error { _, _, err := GenerateGrok(path, ""); return err },
		"Gemini":   func() error { _, _, err := GenerateGemini(path, ""); return err },
		"ACP":      func() error { _, err := ACPServers(path, os.LookupEnv); return err },
		"snapshot": func() error { _, _, err := ReadValidatedSnapshot(path); return err },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("null server panicked instead of returning an error: %v", p)
				}
			}()
			if err := run(); err == nil || !strings.Contains(err.Error(), `MCP server "broken"`) {
				t.Fatalf("null server error = %v, want named invalid definition", err)
			}
		})
	}
}

func TestGenerateGeminiRejectsNullExistingSettings(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("null existing settings panicked: %v", p)
		}
	}()
	if _, _, err := GenerateGemini("", writeTmp(t, "settings.json", "null")); err == nil {
		t.Fatal("present null settings must be rejected before generated defaults overwrite them")
	}
}

func TestBindTaskToolsAcceptsInactiveSnapshots(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", `{"mcpServers":null}`, `{"mcpServers":{}}`} {
		t.Run(raw, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("inactive snapshot panicked: %v", p)
				}
			}()
			encoded, err := BindTaskTools([]byte(raw), "/run/coop/tasks.sock")
			if err != nil {
				t.Fatal(err)
			}
			var bound struct {
				Servers map[string]json.RawMessage `json:"mcpServers"`
			}
			if err := json.Unmarshal(encoded, &bound); err != nil || len(bound.Servers) != 1 {
				t.Fatalf("bound snapshot = %s, %v; want one task server", encoded, err)
			}
		})
	}
}
