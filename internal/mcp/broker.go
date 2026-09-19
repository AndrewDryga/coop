package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// BrokerTokenPrefix names the variables that carry a broker substitute into a box. Coop owns it: a
// shared file cannot reference one, so a stand-in is never mistaken for an operator's token.
const BrokerTokenPrefix = "COOP_MCP_TOKEN_"

// BrokeredServer is where a bearer server is reached from inside a box once Coop brokers it: the
// listener's URL, and the Coop-owned variable holding that listener's substitute.
type BrokeredServer struct{ URL, TokenEnv string }

// CredentialReferences names every variable a snapshot reads a secret from — each
// bearer_token_env_var and each ${VAR} a header value refers to — sorted, once each. These are
// the values a box that keeps MCP credentials outside must never receive.
func CredentialReferences(snapshot []byte) ([]string, error) {
	if len(bytes.TrimSpace(snapshot)) == 0 {
		return nil, nil
	}
	_, servers, err := loadServerViewsData("shared MCP snapshot", snapshot)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, s := range servers {
		if s.BearerTokenEnvVar != "" {
			names = append(names, s.BearerTokenEnvVar)
		}
		for _, value := range s.Headers {
			names = append(names, headerReferences(envValueString(value))...)
		}
	}
	sort.Strings(names)
	return slices.Compact(names), nil
}

// ReferencedCredentialNames is CredentialReferences for a box that never loads the file: read the
// same bounded, no-follow way, names taken from whatever still parses — a broken file must not stop
// a sign-in, and a name it still spells is still kept out.
func ReferencedCredentialNames(path string) []string {
	data, present, err := readConfigFile(path, "shared MCP config")
	if err != nil || !present {
		return nil
	}
	var root struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if json.Unmarshal(data, &root) != nil {
		return nil
	}
	var names []string
	for _, definition := range root.MCPServers {
		if reference, ok := definition["bearer_token_env_var"].(string); ok && reference != "" {
			names = append(names, reference)
		}
		headers, _ := definition["headers"].(map[string]any)
		for _, value := range headers {
			if text, ok := value.(string); ok {
				names = append(names, headerReferences(text)...)
			}
		}
	}
	sort.Strings(names)
	return slices.Compact(names)
}

// headerReferences returns the ${NAME} references inside one header value.
func headerReferences(value string) []string {
	var names []string
	for {
		start := strings.Index(value, "${")
		if start < 0 {
			return names
		}
		end := strings.IndexByte(value[start:], '}')
		if end < 0 {
			return names
		}
		if name := value[start+2 : start+end]; name != "" {
			names = append(names, name)
		}
		value = value[start+end+1:]
	}
}

// RouteThroughBroker points the named bearer servers of a validated snapshot at Coop's broker: a
// server's url becomes its listener's, and its bearer_token_env_var the variable holding that
// listener's substitute. Every other field, and every other server, is kept byte for byte.
func RouteThroughBroker(snapshot []byte, routes map[string]BrokeredServer) ([]byte, error) {
	if len(routes) == 0 {
		return snapshot, nil
	}
	root := map[string]json.RawMessage{}
	if err := json.Unmarshal(snapshot, &root); err != nil {
		return nil, fmt.Errorf("parsing MCP snapshot: %w", err)
	}
	definitions := map[string]json.RawMessage{}
	if err := json.Unmarshal(root["mcpServers"], &definitions); err != nil {
		return nil, fmt.Errorf("parsing MCP snapshot mcpServers: %w", err)
	}
	for name, route := range routes {
		fields := map[string]json.RawMessage{}
		if raw, ok := definitions[name]; !ok || json.Unmarshal(raw, &fields) != nil {
			return nil, fmt.Errorf("MCP snapshot has no server %q to route", name)
		}
		if _, bearer := fields["bearer_token_env_var"]; !bearer {
			return nil, fmt.Errorf("MCP server %q has no bearer token to route", name)
		}
		fields["url"], _ = json.Marshal(route.URL)
		fields["bearer_token_env_var"], _ = json.Marshal(route.TokenEnv)
		definitions[name], _ = json.Marshal(fields)
	}
	return encodeSnapshot(root, definitions)
}

// WithoutRemoteServers is the snapshot an offline box loads: a server reached by URL cannot answer
// without internet, so it is left out, and a local (command) server stays. It returns the names it
// left out, sorted — the snapshot unchanged when there were none.
func WithoutRemoteServers(snapshot []byte) ([]byte, []string, error) {
	if len(bytes.TrimSpace(snapshot)) == 0 {
		return snapshot, nil, nil
	}
	_, servers, err := loadServerViewsData("MCP snapshot", snapshot)
	if err != nil {
		return nil, nil, err
	}
	var remote []string
	for name, s := range servers {
		if s.URL != "" {
			remote = append(remote, name)
		}
	}
	if len(remote) == 0 {
		return snapshot, nil, nil
	}
	sort.Strings(remote)
	root := map[string]json.RawMessage{}
	if err := json.Unmarshal(snapshot, &root); err != nil {
		return nil, nil, fmt.Errorf("parsing MCP snapshot: %w", err)
	}
	definitions := map[string]json.RawMessage{}
	if err := json.Unmarshal(root["mcpServers"], &definitions); err != nil {
		return nil, nil, fmt.Errorf("parsing MCP snapshot mcpServers: %w", err)
	}
	for _, name := range remote {
		delete(definitions, name)
	}
	encoded, err := encodeSnapshot(root, definitions)
	if err != nil {
		return nil, nil, err
	}
	return encoded, remote, nil
}

// ClaudeView is the snapshot as the claude CLI reads it through --mcp-config. The pinned claude
// has no bearer_token_env_var (captured: it sends no Authorization at all) but expands ${VAR} in a
// header, so a bearer server carries `Authorization: Bearer ${VAR}` instead — what the gemini and
// grok renderings already do.
func ClaudeView(snapshot []byte) ([]byte, error) {
	root := map[string]json.RawMessage{}
	if err := json.Unmarshal(snapshot, &root); err != nil {
		return nil, fmt.Errorf("parsing MCP snapshot: %w", err)
	}
	definitions := map[string]json.RawMessage{}
	if err := json.Unmarshal(root["mcpServers"], &definitions); err != nil {
		return nil, fmt.Errorf("parsing MCP snapshot mcpServers: %w", err)
	}
	for name, raw := range definitions {
		fields := map[string]json.RawMessage{}
		if json.Unmarshal(raw, &fields) != nil {
			return nil, fmt.Errorf("parsing MCP server %q", name)
		}
		var reference string
		if json.Unmarshal(fields["bearer_token_env_var"], &reference) != nil || reference == "" {
			continue
		}
		headers := map[string]json.RawMessage{}
		if raw, ok := fields["headers"]; ok && json.Unmarshal(raw, &headers) != nil {
			return nil, fmt.Errorf("parsing MCP server %q headers", name)
		}
		delete(fields, "bearer_token_env_var")
		headers["Authorization"], _ = json.Marshal("Bearer ${" + reference + "}")
		fields["headers"], _ = json.Marshal(headers)
		definitions[name], _ = json.Marshal(fields)
	}
	return encodeSnapshot(root, definitions)
}

// encodeSnapshot re-encodes a snapshot around its rewritten servers and proves the result still
// crosses the validation every renderer relies on.
func encodeSnapshot(root, definitions map[string]json.RawMessage) ([]byte, error) {
	var err error
	if root["mcpServers"], err = json.Marshal(definitions); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > maxMCPConfigBytes {
		return nil, errors.New("MCP snapshot outgrew its size limit")
	}
	if _, _, err := loadServerViewsData("MCP snapshot", encoded); err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
