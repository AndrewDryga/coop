package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

// BrokerTokenPrefix names the variables that carry a broker substitute into a box. Coop owns it: a
// shared file cannot reference one, so a stand-in is never mistaken for an operator's token.
const BrokerTokenPrefix = "COOP_MCP_TOKEN_"

// BrokeredServer is where a secret-bearing server is reached from inside a box once Coop brokers
// it: the listener's URL, the Coop-owned variable holding that listener's substitute, and — for a
// header secret — the header key as the file spells it and the literal text before the secret. A
// bearer server leaves HeaderKey empty.
type BrokeredServer struct{ URL, TokenEnv, HeaderKey, Prefix string }

// SecretServer is one remote server that authenticates with a secret its box must not hold: the
// header that carries it (lower-case, as the broker names it) and its key as the file spells it,
// the literal text before the secret, the variable the secret is read from, and whether that is a
// bearer_token_env_var (an Authorization header written out reads the same on the wire, but is
// rewritten where it is written).
type SecretServer struct {
	Name, URL, Transport                string
	Header, HeaderKey, Prefix, Variable string
	Bearer                              bool
}

// SecretServers lists a validated snapshot's remote servers that carry a secret, sorted by name: a
// bearer_token_env_var (the Authorization header after "Bearer "), or one header whose whole value
// is literal text followed by one ${VARIABLE}. A literal header is not a secret and stays as
// written. Any other shape — two secrets, text after the reference, two references in one value —
// is refused by name: no single brokered header could carry it.
func SecretServers(snapshot []byte) ([]SecretServer, error) {
	if len(bytes.TrimSpace(snapshot)) == 0 {
		return nil, nil
	}
	_, servers, err := loadServerViewsData("MCP snapshot", snapshot)
	if err != nil {
		return nil, err
	}
	var result []SecretServer
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		s := servers[name]
		if s.URL == "" {
			continue
		}
		var secrets []SecretServer
		if s.BearerTokenEnvVar != "" {
			secrets = append(secrets, SecretServer{Header: "authorization", HeaderKey: "Authorization", Prefix: "Bearer ", Variable: s.BearerTokenEnvVar, Bearer: true})
		}
		for _, key := range slices.Sorted(maps.Keys(s.Headers)) {
			value := envValueString(s.Headers[key])
			references := headerReferences(value)
			if len(references) == 0 {
				continue
			}
			prefix, _, _ := strings.Cut(value, "${")
			if len(references) != 1 || value != prefix+"${"+references[0]+"}" {
				return nil, fmt.Errorf("MCP server %q's %s header must be literal text then one ${VARIABLE}, so Coop can keep the secret outside the box", name, key)
			}
			secrets = append(secrets, SecretServer{Header: strings.ToLower(key), HeaderKey: key, Prefix: prefix, Variable: references[0]})
		}
		if len(secrets) == 0 {
			continue
		}
		if len(secrets) > 1 {
			return nil, fmt.Errorf("MCP server %q carries %d secrets; Coop can keep one per server outside the box", name, len(secrets))
		}
		transport := s.Type
		if transport == "" {
			transport = "http"
		}
		secret := secrets[0]
		secret.Name, secret.URL, secret.Transport = name, s.URL, transport
		result = append(result, secret)
	}
	return result, nil
}

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

// RouteThroughBroker points the named secret-bearing servers of a validated snapshot at Coop's
// broker: a server's url becomes its listener's, and its secret the variable holding that
// listener's substitute — the bearer_token_env_var itself, or the secret header's ${…} with its
// prefix kept. Every other field, and every other server, is kept byte for byte.
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
		fields["url"], _ = json.Marshal(route.URL)
		if route.HeaderKey == "" {
			if _, bearer := fields["bearer_token_env_var"]; !bearer {
				return nil, fmt.Errorf("MCP server %q has no bearer token to route", name)
			}
			fields["bearer_token_env_var"], _ = json.Marshal(route.TokenEnv)
		} else {
			headers := map[string]json.RawMessage{}
			if raw, ok := fields["headers"]; !ok || json.Unmarshal(raw, &headers) != nil || headers[route.HeaderKey] == nil {
				return nil, fmt.Errorf("MCP server %q has no %s header to route", name, route.HeaderKey)
			}
			headers[route.HeaderKey], _ = json.Marshal(route.Prefix + "${" + route.TokenEnv + "}")
			fields["headers"], _ = json.Marshal(headers)
		}
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
