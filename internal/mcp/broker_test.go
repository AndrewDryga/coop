package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const brokerFixture = `{"mcpServers":{
  "emisar":{"type":"http","url":"https://emisar.example/mcp","bearer_token_env_var":"EMISAR_TOKEN","headers":{"X-Tenant":"ops"}},
  "docs":{"type":"http","url":"https://docs.example/mcp","headers":{"X-Key":"${DOCS_KEY}","X-Trace":"id-${TRACE_ID}-x"}},
  "tasks":{"command":"socat","args":["STDIO","UNIX-CONNECT:/run/tasks.sock"],"env":{"PORT":8080}}
}}`

// Every variable a snapshot reads a secret from is named once: bearer references and each ${VAR}
// inside a header, wherever it sits in the value.
func TestCredentialReferencesNameEverySecretVariable(t *testing.T) {
	names, err := CredentialReferences([]byte(brokerFixture))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"DOCS_KEY", "EMISAR_TOKEN", "TRACE_ID"}; !slices.Equal(names, want) {
		t.Fatalf("references = %q, want %q", names, want)
	}
	if names, err := CredentialReferences(nil); err != nil || names != nil {
		t.Fatalf("an absent snapshot referenced %q, %v", names, err)
	}
}

// Routing rewrites exactly the named bearer server — its url and its token variable — and keeps
// every other field and server as it was, numbers included.
func TestRouteThroughBrokerRewritesOnlyTheRoutedServer(t *testing.T) {
	routed, err := RouteThroughBroker([]byte(brokerFixture), map[string]BrokeredServer{
		"emisar": {URL: "http://127.0.0.1:15580/mcp", TokenEnv: "COOP_MCP_TOKEN_0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(routed, &root); err != nil {
		t.Fatal(err)
	}
	emisar := root.MCPServers["emisar"]
	if emisar["url"] != "http://127.0.0.1:15580/mcp" || emisar["bearer_token_env_var"] != "COOP_MCP_TOKEN_0" ||
		emisar["headers"].(map[string]any)["X-Tenant"] != "ops" || emisar["type"] != "http" {
		t.Fatalf("routed server = %#v", emisar)
	}
	if root.MCPServers["docs"]["url"] != "https://docs.example/mcp" || !strings.Contains(string(routed), `"PORT":8080`) {
		t.Fatalf("an unrouted server changed:\n%s", routed)
	}
	for _, bad := range []map[string]BrokeredServer{{"missing": {URL: "x", TokenEnv: "X"}}, {"docs": {URL: "x", TokenEnv: "X"}}} {
		if _, err := RouteThroughBroker([]byte(brokerFixture), bad); err == nil {
			t.Fatalf("routed %v", bad)
		}
	}
}

// The pinned claude reads a bearer server only as a header it expands, so its view carries
// Authorization: Bearer ${VAR} in place of bearer_token_env_var, beside the server's other headers.
func TestClaudeViewTurnsABearerReferenceIntoAHeader(t *testing.T) {
	view, err := ClaudeView([]byte(brokerFixture))
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(view, &root); err != nil {
		t.Fatal(err)
	}
	emisar := root.MCPServers["emisar"]
	headers, _ := emisar["headers"].(map[string]any)
	if _, bearer := emisar["bearer_token_env_var"]; bearer || headers["Authorization"] != "Bearer ${EMISAR_TOKEN}" || headers["X-Tenant"] != "ops" {
		t.Fatalf("claude view of a bearer server = %#v", emisar)
	}
	if _, ok := root.MCPServers["docs"]["headers"].(map[string]any)["Authorization"]; ok {
		t.Fatal("a server without a bearer reference gained an Authorization header")
	}
}

// The broker's stand-ins live under a prefix an operator's file cannot reference, as a bearer
// variable or inside a header.
func TestSharedConfigCannotReferenceABrokerStandIn(t *testing.T) {
	for _, definition := range []string{
		`{"type":"http","url":"https://x.example/mcp","bearer_token_env_var":"COOP_MCP_TOKEN_0"}`,
		`{"type":"http","url":"https://x.example/mcp","headers":{"X-Key":"${COOP_MCP_TOKEN_3}"}}`,
	} {
		path := filepath.Join(t.TempDir(), "mcp.json")
		if err := os.WriteFile(path, []byte(`{"mcpServers":{"x":`+definition+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadValidatedSnapshot(path); err == nil || !strings.Contains(err.Error(), BrokerTokenPrefix) {
			t.Fatalf("a reference to a broker stand-in was accepted: %v", err)
		}
	}
}
