package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
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
		`{"mcpServers":{"tool":{"url":"https://mcp.example.com","headers":{"${TENANT}":"first"}}}}`,
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

// A person with several servers needs a refusal to say which one to change, and for a setting
// which setting. It never repeats what the setting holds: a header can carry a secret.
func TestNetworkServersRefuseByServerName(t *testing.T) {
	good := func(name string) string { return `"` + name + `":{"url":"https://mcp.example.com"}` }
	for name, tc := range map[string]struct {
		server string
		want   []string
		never  []string
	}{
		"a header that uses a variable": {
			`"qa-hdr":{"url":"https://mcp.example.com","headers":{"X-Api-Key":"${QA_MCP_TOKEN}"}}`,
			[]string{`MCP server "qa-hdr"`, `header "X-Api-Key"`, "bearer_token_env_var"},
			[]string{"QA_MCP_TOKEN"},
		},
		"a header with a secret beside its variable": {
			`"qa-hdr":{"url":"https://mcp.example.com","headers":{"Authorization":"Bearer sk-live-4242${SUFFIX}"}}`,
			[]string{`MCP server "qa-hdr"`, `header "Authorization"`},
			[]string{"sk-live-4242", "SUFFIX"},
		},
		"a url that uses a variable": {
			`"qa-url":{"url":"https://${HOST}/mcp"}`,
			[]string{`MCP server "qa-url"`, `"url"`},
			[]string{"HOST", "bearer_token_env_var"}, // a header's remedy does not fit a url
		},
		"an argument that uses a variable": {
			`"qa-args":{"command":"tool","args":["--quiet","${ROUTE}"]}`,
			[]string{`MCP server "qa-args"`, `"args[1]"`},
			[]string{"ROUTE"},
		},
		"an environment value that uses a variable": {
			`"qa-env":{"command":"tool","env":{"TENANT":"${TENANT}"}}`,
			[]string{`MCP server "qa-env"`, `"env.TENANT"`},
			nil,
		},
		"a url that is not https": {
			`"qa-http":{"url":"http://mcp.example.com"}`,
			[]string{`MCP server "qa-http"`, "literal HTTPS origin"},
			nil,
		},
		"a helper that signs in": {
			`"qa-helper":{"url":"https://mcp.example.com","headersHelper":"helper"}`,
			[]string{`MCP server "qa-helper"`, "headersHelper"},
			nil,
		},
		"a transport it cannot route": {
			`"qa-ws":{"type":"websocket","url":"https://mcp.example.com"}`,
			[]string{`MCP server "qa-ws"`, "transport"},
			nil,
		},
		"a server with nothing to start": {
			`"qa-empty":{"type":"http"}`,
			[]string{`MCP server "qa-empty"`, "no usable transport"},
			nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Good servers on both sides of the bad one, so the answer cannot be the first or the last.
			snapshot := `{"mcpServers":{` + good("a-good") + `,` + tc.server + `,` + good("z-good") + `}}`
			_, err := NetworkServers([]byte(snapshot))
			if err == nil {
				t.Fatalf("a filtered box took %s", snapshot)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not say %q", err, want)
				}
			}
			for _, never := range append([]string{`"a-good"`, `"z-good"`}, tc.never...) {
				if strings.Contains(err.Error(), never) {
					t.Errorf("refusal %q says %q", err, never)
				}
			}
		})
	}
}

func TestNetworkShapeExcludesRotatingBearerEnvironmentValue(t *testing.T) {
	// The reference identifies the routing shape; only its value rotates, and a
	// renewal must not look like a configuration change requiring requalification.
	snapshot := `{"mcpServers":{"tool":{"url":"https://mcp.example.com","bearer_token_env_var":"TOOL_TOKEN"}}}`
	t.Setenv("TOOL_TOKEN", "first-secret-value")
	first := networkShape(t, snapshot)
	if bytes.Contains(first, []byte("first-secret-value")) || !bytes.Contains(first, []byte("bearer-env")) || !bytes.Contains(first, []byte("TOOL_TOKEN")) {
		t.Fatal("bearer projection is wrong", string(first))
	}
	t.Setenv("TOOL_TOKEN", "second-secret-value")
	second := networkShape(t, snapshot)
	if !bytes.Equal(first, second) || bytes.Contains(second, []byte("second-secret-value")) {
		t.Fatal("shape is not stable")
	}
	if bytes.Equal(first, networkShape(t, strings.ReplaceAll(snapshot, "TOOL_TOKEN", "OTHER_TOKEN"))) {
		t.Fatal("bearer reference change disappeared")
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
