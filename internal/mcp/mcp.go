// Package mcp turns one shared mcp.json (the standard {"mcpServers": {...}}
// shape) into each agent's native MCP configuration. Claude reads mcp.json
// directly via --mcp-config, so only Gemini and Codex need translation:
//
//   - Gemini: merge the servers into its settings.json (same JSON shape).
//   - Codex:  emit [mcp_servers.*] tables in its config.toml.
//
// Both are generated on top of the user's existing config (never mutating it),
// with servers from mcp.json winning on a name clash. Output is deterministic
// (servers sorted by name) so it is stable across runs and easy to test.
package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// server is the typed view of one entry, sufficient to emit Codex TOML.
type server struct {
	Command           string         `json:"command"`
	Args              []string       `json:"args"`
	Env               map[string]any `json:"env"`
	URL               string         `json:"url"`
	BearerTokenEnvVar string         `json:"bearer_token_env_var"`
}

// GenerateGemini merges the shared servers into the user's Gemini settings.json,
// preserving every other setting. existing may be "" or a missing file.
func GenerateGemini(mcpFile, existing string) (string, error) {
	servers, err := loadServersAny(mcpFile)
	if err != nil {
		return "", err
	}
	settings := readJSONObject(existing)

	merged, _ := settings["mcpServers"].(map[string]any)
	if merged == nil {
		merged = map[string]any{}
	}
	for name, def := range servers {
		merged[name] = def
	}
	settings["mcpServers"] = merged

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(settings); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// GenerateCodex emits the shared servers as [mcp_servers.*] tables for Codex's
// config.toml, preserving everything in the user's existing config except its
// own [mcp_servers.*] tables (mcp.json is authoritative for MCP).
func GenerateCodex(mcpFile, existing string) (string, error) {
	servers, err := loadServersTyped(mcpFile)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(keepNonMCP(existing))
	for _, name := range sortedKeys(servers) {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		writeCodexServer(&b, name, servers[name])
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", nil
	}
	return out + "\n", nil
}

// envValueString renders an MCP env value as the string Codex (and the shell) will see. MCP env is
// string→string, but JSON numbers decode to float64, and fmt.Sprint gives a float64 scientific
// notation (12345 → fine, but a big value → "1.23e+19"). Format floats with 'f' so a numeric env
// value renders as plain digits (a port "8080", not "8080" via "8.08e+03"). Non-numbers pass through.
func envValueString(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func writeCodexServer(b *strings.Builder, name string, s server) {
	fmt.Fprintf(b, "[mcp_servers.%s]\n", name)
	switch {
	case s.URL != "": // streamable HTTP server
		fmt.Fprintf(b, "url = %s\n", tomlString(s.URL))
		if s.BearerTokenEnvVar != "" {
			fmt.Fprintf(b, "bearer_token_env_var = %s\n", tomlString(s.BearerTokenEnvVar))
		}
	case s.Command != "": // stdio server
		fmt.Fprintf(b, "command = %s\n", tomlString(s.Command))
		if len(s.Args) > 0 {
			fmt.Fprintf(b, "args = %s\n", tomlStringArray(s.Args))
		}
		if len(s.Env) > 0 {
			b.WriteString("\n")
			fmt.Fprintf(b, "[mcp_servers.%s.env]\n", name)
			for _, k := range sortedKeys(s.Env) {
				fmt.Fprintf(b, "%s = %s\n", k, tomlString(envValueString(s.Env[k])))
			}
		}
	}
}

// keepNonMCP returns the user's config.toml with its [mcp_servers.*] tables
// removed (and trailing blank lines trimmed), or "" if there is no such file.
// Only real MCP tables are stripped — [mcp_servers.<name>...] and a bare [mcp_servers] —
// NOT a lookalike like [mcp_servers_backup] or [mcp_serverssettings], which a too-broad
// prefix used to silently drop from the box's config.
func keepNonMCP(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var kept []string
	skip := false
	for _, line := range strings.Split(string(data), "\n") {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "[") {
			skip = strings.HasPrefix(s, "[mcp_servers.") || strings.HasPrefix(s, "[mcp_servers]")
		}
		if !skip {
			kept = append(kept, strings.TrimRight(line, "\r"))
		}
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

func loadServersAny(path string) (map[string]any, error) {
	var f struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := readMCP(path, &f); err != nil {
		return nil, err
	}
	if f.MCPServers == nil {
		return map[string]any{}, nil
	}
	return f.MCPServers, nil
}

func loadServersTyped(path string) (map[string]server, error) {
	var f struct {
		MCPServers map[string]server `json:"mcpServers"`
	}
	if err := readMCP(path, &f); err != nil {
		return nil, err
	}
	return f.MCPServers, nil
}

func readMCP(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

// readJSONObject reads a JSON object, tolerating a missing or malformed file by
// returning an empty object (so a broken settings.json never aborts a run).
func readJSONObject(path string) map[string]any {
	out := map[string]any{}
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func tomlStringArray(a []string) string {
	parts := make([]string, len(a))
	for i, x := range a {
		parts[i] = tomlString(x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
