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

// An offline box keeps its local servers and loses every server reached by URL, whatever its auth;
// a snapshot with nothing remote comes back byte for byte.
func TestWithoutRemoteServersKeepsOnlyLocalOnes(t *testing.T) {
	snapshot := []byte(`{"mcpServers":{
		"local":{"command":"true","args":["x"]},
		"remote":{"type":"http","url":"https://remote.example/mcp","bearer_token_env_var":"REMOTE_TOKEN"},
		"legacy":{"type":"sse","url":"https://legacy.example/sse"},
		"headers":{"url":"https://headers.example/mcp","headers":{"X-Key":"${HEADER_KEY}"}}}}`)
	local, omitted, err := WithoutRemoteServers(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"headers", "legacy", "remote"}; !slices.Equal(omitted, want) {
		t.Fatalf("omitted = %q, want %q", omitted, want)
	}
	if strings.Contains(string(local), "example") || !strings.Contains(string(local), `"local"`) {
		t.Fatalf("offline snapshot = %s", local)
	}
	onlyLocal := []byte(`{"mcpServers":{"local":{"command":"true"}}}`)
	if kept, none, err := WithoutRemoteServers(onlyLocal); err != nil || none != nil || string(kept) != string(onlyLocal) {
		t.Fatalf("an all-local snapshot changed: %s, %q, %v", kept, none, err)
	}
}

// A server's secret is a bearer_token_env_var or one header that is literal text then one ${VAR};
// a literal header is no secret, and any other shape comes back as unbrokerable, by name, with every
// variable it reads.
func TestSecretServersReadsEachSecretShape(t *testing.T) {
	servers, unbrokerable, err := SecretServers([]byte(`{"mcpServers":{
		"bearer":{"url":"https://a.example/mcp","bearer_token_env_var":"A_TOKEN"},
		"key":{"url":"https://b.example/mcp","headers":{"X-Api-Key":"${B_KEY}","X-Client":"coop"}},
		"token":{"type":"http","url":"https://c.example/mcp","headers":{"Authorization":"Token ${C_TOKEN}"}},
		"plain":{"url":"https://d.example/mcp","headers":{"X-Client":"coop"}},
		"local":{"command":"true"}}}`))
	if err != nil || unbrokerable != nil {
		t.Fatal(unbrokerable, err)
	}
	want := []SecretServer{
		{Name: "bearer", URL: "https://a.example/mcp", Transport: "http", Header: "authorization", HeaderKey: "Authorization", Prefix: "Bearer ", Variable: "A_TOKEN", Bearer: true},
		{Name: "key", URL: "https://b.example/mcp", Transport: "http", Header: "x-api-key", HeaderKey: "X-Api-Key", Variable: "B_KEY"},
		{Name: "token", URL: "https://c.example/mcp", Transport: "http", Header: "authorization", HeaderKey: "Authorization", Prefix: "Token ", Variable: "C_TOKEN"},
	}
	if !slices.Equal(servers, want) {
		t.Fatalf("secret servers = %+v\nwant %+v", servers, want)
	}
	for name, test := range map[string]struct {
		definition string
		variables  []string
	}{
		"text after the reference": {`{"url":"https://x.example/mcp","headers":{"X-Key":"${K}-suffix"}}`, []string{"K"}},
		"two references":           {`{"url":"https://x.example/mcp","headers":{"X-Key":"${K}${L}"}}`, []string{"K", "L"}},
		"two secrets":              {`{"url":"https://x.example/mcp","bearer_token_env_var":"K","headers":{"X-Key":"${L}"}}`, []string{"K", "L"}},
	} {
		servers, unbrokerable, err := SecretServers([]byte(`{"mcpServers":{"x":` + test.definition + `}}`))
		if err != nil || servers != nil || len(unbrokerable) != 1 || unbrokerable[0].Name != "x" ||
			!strings.Contains(unbrokerable[0].Reason, `"x"`) || !slices.Equal(unbrokerable[0].Variables, test.variables) {
			t.Errorf("%s: %+v, %+v, %v", name, servers, unbrokerable, err)
		}
	}
}

// A header secret is rewritten where it is written — the key keeps its spelling, the prefix stays —
// and nothing else in the server changes.
func TestRouteThroughBrokerRewritesASecretHeader(t *testing.T) {
	snapshot := []byte(`{"mcpServers":{"key":{"url":"https://b.example/mcp","headers":{"X-Api-Key":"key ${B_KEY}","X-Client":"coop"}}}}`)
	routed, err := RouteThroughBroker(snapshot, map[string]BrokeredServer{
		"key": {URL: "http://127.0.0.1:15581/mcp", TokenEnv: "COOP_MCP_TOKEN_1", HeaderKey: "X-Api-Key", Prefix: "key "},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"url":"http://127.0.0.1:15581/mcp"`, `"X-Api-Key":"key ${COOP_MCP_TOKEN_1}"`, `"X-Client":"coop"`} {
		if !strings.Contains(string(routed), want) {
			t.Errorf("routed snapshot lacks %s:\n%s", want, routed)
		}
	}
	if strings.Contains(string(routed), "B_KEY") || strings.Contains(string(routed), "b.example") {
		t.Fatalf("the operator's variable or host survived:\n%s", routed)
	}
}
