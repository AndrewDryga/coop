package mcp

import (
	"bytes"
	"encoding/json"
	"testing"
)

func networkShape(t *testing.T, snapshot string) []byte {
	t.Helper()
	servers, err := NetworkServers([]byte(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(servers)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNetworkShapeBindsConsumedConfiguration(t *testing.T) {
	for _, pair := range [][2]string{
		{`{"mcpServers":{"tool":{"url":"https://mcp.example.com","headers":{"X-Tenant":"first"}}}}`, `{"mcpServers":{"tool":{"url":"https://mcp.example.com","headers":{"X-Tenant":"second"}}}}`},
		{`{"mcpServers":{"tool":{"command":"tool","extension":{"route":"first"}}}}`, `{"mcpServers":{"tool":{"command":"tool","extension":{"route":"second"}}}}`},
	} {
		if bytes.Equal(networkShape(t, pair[0]), networkShape(t, pair[1])) {
			t.Fatal("consumed setting omitted from projection")
		}
	}
	for _, snapshot := range []string{
		`{"mcpServers":{"tool":{"url":"https://mcp.example.com","headersHelper":"helper"}}}`,
		`{"mcpServers":{"tool":{"url":"https://mcp.example.com","oauth":{"clientId":"other"}}}}`,
		`{"mcpServers":{"tool":{"url":"https://mcp.example.com","headers":{"X-Tenant":"${TENANT}"}}}}`,
		`{"mcpServers":{"tool":{"url":"https://mcp.example.com","headers":{"X-Tenant":"${TENANT}"}}}}`,
		`{"mcpServers":{"tool":{"command":"tool","args":["${ROUTE}"]}}}`,
		`{"mcpServers":{"tool":{"url":"http://mcp.example.com"}}}`,
		`{"mcpServers":{"tool":{"url":"https://user:secret@mcp.example.com"}}}`,
		`{"mcpServers":{"tool":{"url":"https://mcp.example.com?tenant=first"}}}`,
		`{"mcpServers":{"tool":{"url":"https://mcp.example.com:8443"}}}`,
	} {
		if _, err := NetworkServers([]byte(snapshot)); err == nil {
			t.Fatal("unqualified MCP routing accepted:", snapshot)
		}
	}
}

func TestNetworkShapeExcludesRotatingBearerEnvironmentValue(t *testing.T) {
	// The reference identifies the routing shape; only its value rotates, and a
	// renewal must not look like a configuration change requiring requalification.
	snapshot := `{"mcpServers":{"tool":{"url":"https://mcp.example.com","bearer_token_env_var":"TOOL_TOKEN"}}}`
	first := networkShape(t, snapshot)
	if bytes.Contains(first, []byte("TOOL_TOKEN_VALUE")) || !bytes.Contains(first, []byte("bearer-env")) {
		t.Fatal("bearer projection is wrong", string(first))
	}
	if !bytes.Equal(first, networkShape(t, snapshot)) {
		t.Fatal("shape is not stable")
	}
}

func TestNetworkServersOnlyReportsHTTPDestinations(t *testing.T) {
	servers, err := NetworkServers([]byte(`{"mcpServers":{"local":{"command":"tool"},"remote":{"url":"https://mcp.example.com"}}}`))
	if err != nil || len(servers) != 2 {
		t.Fatal(servers, err)
	}
	if servers[0].Name != "local" || servers[0].Transport != "stdio" || servers[0].URL != "" {
		t.Fatal("a local command implied a destination", servers[0])
	}
	if servers[1].URL != "https://mcp.example.com" || servers[1].Transport != "http" {
		t.Fatal("HTTP destination lost", servers[1])
	}
	if got, err := NetworkServers(nil); err != nil || got != nil {
		t.Fatal("absent MCP configuration derived dependencies", got, err)
	}
}
