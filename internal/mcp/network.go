package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// NetworkServer is the private routing shape of one enabled MCP server: what a
// restricted run must reach, and what identifies the configuration it was
// qualified against. Definition may hold sensitive routing headers, so this is
// never public telemetry. Bearer VALUES are deliberately absent, so token
// renewal does not change the shape. A stdio command implies no network grant.
type NetworkServer struct {
	Name            string          `json:"name"`
	Transport       string          `json:"transport"`
	URL             string          `json:"url,omitempty"`
	Auth            string          `json:"auth"`
	BearerReference string          `json:"bearer_reference,omitempty"`
	Headers         []string        `json:"headers,omitempty"`
	Definition      json.RawMessage `json:"definition"`
}

// NetworkServers reads an already validated shared snapshot. It refuses a
// configuration whose destination cannot be read literally rather than guessing
// a host from an interpolated or credential-bearing URL.
func NetworkServers(snapshot []byte) ([]NetworkServer, error) {
	if len(bytes.TrimSpace(snapshot)) == 0 {
		return nil, nil
	}
	_, servers, err := loadServerViewsData("shared MCP snapshot", snapshot)
	if err != nil {
		return nil, err
	}
	if len(servers) > 64 {
		return nil, errors.New("restricted MCP inventory exceeds its server limit")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(snapshot, &root); err != nil {
		return nil, err
	}
	var definitions map[string]json.RawMessage
	if err := json.Unmarshal(root["mcpServers"], &definitions); err != nil {
		return nil, err
	}
	var result []NetworkServer
	for _, name := range sortedKeys(servers) {
		shape, err := networkServer(name, servers[name], definitions[name])
		if err != nil {
			// A person with several servers needs to know which one to change.
			return nil, fmt.Errorf("MCP server %q: %w", name, err)
		}
		result = append(result, shape)
	}
	return result, nil
}

// networkServer is one server's routing shape, or why a restricted run cannot take it. Its errors
// leave out the server's name: NetworkServers adds it to every one.
func networkServer(name string, server server, raw json.RawMessage) (NetworkServer, error) {
	shape := NetworkServer{Name: name, Transport: server.Type, Auth: "none"}
	// Bind the raw object, including routing headers and local command
	// extensions, not the smaller typed view that would discard them.
	var definition map[string]json.RawMessage
	if err := json.Unmarshal(raw, &definition); err != nil {
		return NetworkServer{}, err
	}
	if err := literalNetworkDefinition(raw); err != nil {
		return NetworkServer{}, err
	}
	for _, key := range []string{"headersHelper", "oauth"} {
		if value, exists := definition[key]; exists && string(value) != "null" {
			return NetworkServer{}, fmt.Errorf("restricted networking does not support signing in through %s", key)
		}
	}
	var err error
	if shape.Definition, err = json.Marshal(definition); err != nil {
		return NetworkServer{}, err
	}
	switch {
	case server.URL != "":
		parsed, err := url.Parse(server.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" ||
			parsed.RawQuery != "" || parsed.Opaque != "" || parsed.Port() != "" && parsed.Port() != "443" || strings.ContainsAny(server.URL, "${\x00\r\n") {
			return NetworkServer{}, errors.New("restricted MCP requires a literal HTTPS origin without embedded credentials or query routing")
		}
		if shape.Transport == "" {
			shape.Transport = "http"
		}
		if shape.Transport != "http" && shape.Transport != "sse" || server.Command != "" {
			return NetworkServer{}, errors.New("unsupported or ambiguous restricted MCP transport")
		}
		shape.URL = server.URL
		if server.BearerTokenEnvVar != "" {
			if !validBearerReference(server.BearerTokenEnvVar) {
				return NetworkServer{}, errors.New("unsupported restricted MCP bearer reference")
			}
			shape.Auth, shape.BearerReference = "bearer-env", server.BearerTokenEnvVar
		}
		shape.Headers = sortedKeys(server.Headers)
		if len(shape.Headers) > 0 && shape.Auth == "none" {
			shape.Auth = "headers"
		}
	case server.Command != "":
		if shape.Transport != "" && shape.Transport != "stdio" {
			return NetworkServer{}, errors.New("unsupported restricted MCP local transport")
		}
		shape.Transport = "stdio"
	default:
		return NetworkServer{}, errors.New("restricted MCP server has no usable transport")
	}
	return shape, nil
}

// Native interpolation can change routing without changing configuration bytes.
// The qualified subset is literal configuration plus an explicit bearer
// reference, whose value each consumer resolves from its own environment. The
// refusal names the setting, never what it holds: a header can carry a secret.
func literalNetworkDefinition(data []byte) error {
	var definition any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&definition); err != nil {
		return errors.New("invalid restricted MCP definition")
	}
	at, found := interpolatedAt(definition, "")
	if !found {
		return nil
	}
	if header, isHeader := strings.CutPrefix(at, "headers."); isHeader {
		return fmt.Errorf("header %q uses ${…}, but a filtered box needs literal MCP configuration; use bearer_token_env_var for rotating authentication", header)
	}
	return fmt.Errorf("%q uses ${…}, but a filtered box needs literal MCP configuration", at)
}

// interpolatedAt finds the first string, or object key, of a decoded JSON value that holds ${. It
// answers with the keys that lead there joined by dots, and array positions in brackets. Keys are
// read in sorted order, so one file always gets the same answer.
func interpolatedAt(value any, at string) (string, bool) {
	switch v := value.(type) {
	case string:
		return at, strings.Contains(v, "${")
	case []any:
		for i, item := range v {
			if found, ok := interpolatedAt(item, fmt.Sprintf("%s[%d]", at, i)); ok {
				return found, true
			}
		}
	case map[string]any:
		for _, key := range sortedKeys(v) {
			child := key
			if at != "" {
				child = at + "." + key
			}
			if strings.Contains(key, "${") {
				return child, true
			}
			if found, ok := interpolatedAt(v[key], child); ok {
				return found, true
			}
		}
	}
	return "", false
}

func validBearerReference(name string) bool {
	if name == "" || len(name) > 128 || name == "COOP_RUN_ID" {
		return false
	}
	return strings.Trim(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") == "" && !strings.ContainsRune("0123456789", rune(name[0]))
}
