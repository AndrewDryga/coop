// Package mcp turns one shared mcp.json (the standard {"mcpServers": {...}}
// shape) into each agent's native MCP configuration:
//
//   - Gemini: merge the servers into its settings.json (same JSON shape).
//   - Codex:  emit [mcp_servers.*] tables in its config.toml.
//   - ACP:    pass the servers to session/new, which takes them as a parameter.
//
// An ACP adapter takes no flags, so a file it could be pointed at is no use to it. The claude
// CLI needs no translation at all — it reads mcp.json directly via --mcp-config.
//
// The generated files are written on top of the user's existing config (never mutating it).
// When shared MCP is active, native MCP declarations are removed or refused so mcp.json remains
// the only server authority. Output is deterministic (servers sorted by name) so it is stable
// across runs and easy to test.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

const maxMCPConfigBytes int64 = 4 << 20

// server is the typed view of one entry, sufficient to emit Codex TOML and the ACP parameter.
// Headers is the canonical HTTP-auth field claude + gemini read directly; codex can't use it (it
// has no inline-header support, only bearer_token_env_var / OAuth), so for codex it's kept here
// only to flag that gap, not to emit. Type distinguishes "sse" from the "http" default; it is
// absent on a stdio server and must stay absent in the ACP shape (see acpServer).
type server struct {
	Type              string         `json:"type"`
	Command           string         `json:"command"`
	Args              []string       `json:"args"`
	Env               map[string]any `json:"env"`
	URL               string         `json:"url"`
	Headers           map[string]any `json:"headers"`
	BearerTokenEnvVar string         `json:"bearer_token_env_var"`
}

// GenerateGemini builds the Gemini settings mounted inside a box, preserving the user's
// settings while forcing box-safe file filtering. A non-empty mcpFile also merges the shared
// servers; "" leaves the user's mcpServers untouched. existing may be "" or a missing file.
func GenerateGemini(mcpFile, existing string) (string, error) {
	settings, err := readJSONObject(existing)
	if err != nil {
		return "", err
	}

	if mcpFile != "" {
		servers, err := loadServersAny(mcpFile)
		if err != nil {
			return "", err
		}
		merged, _ := settings["mcpServers"].(map[string]any)
		if merged == nil {
			merged = map[string]any{}
		}
		for name, def := range servers {
			merged[name] = def
		}
		settings["mcpServers"] = merged
	}

	contextSettings, _ := settings["context"].(map[string]any)
	if contextSettings == nil {
		contextSettings = map[string]any{}
	}
	fileFiltering, _ := contextSettings["fileFiltering"].(map[string]any)
	if fileFiltering == nil {
		fileFiltering = map[string]any{}
	}
	fileFiltering["respectGitIgnore"] = false
	contextSettings["fileFiltering"] = fileFiltering
	settings["context"] = contextSettings

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
	native, err := keepNonMCP(existing)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(native)
	for _, name := range sortedKeys(servers) {
		server := servers[name]
		if server.URL == "" && server.Command == "" {
			continue
		}
		separateTOMLBlock(&b)
		writeCodexServer(&b, name, server)
	}
	return b.String(), nil
}

func separateTOMLBlock(b *strings.Builder) {
	if b.Len() == 0 {
		return
	}
	s := b.String()
	switch {
	case strings.HasSuffix(s, "\n\n"):
	case strings.HasSuffix(s, "\n"):
		b.WriteByte('\n')
	default:
		b.WriteString("\n\n")
	}
}

// ACPServers renders the shared servers as the list an ACP session/new (and the identical
// session/load) carries in its "mcpServers" parameter. The adapter is the only consumer, and it
// wants name/value PAIR LISTS where mcp.json has objects, so this is a translation, not a
// passthrough (verified against @agentclientprotocol/claude-agent-acp 0.68.0):
//
//	http/sse: {"type":"http"|"sse","name":…,"url":…,"headers":[{"name":…,"value":…}]}
//	stdio:    {"name":…,"command":…,"args":[…],"env":[{"name":…,"value":…}]}
//
// lookupEnv resolves bearer_token_env_var, which the adapter has no equivalent for — it accepts
// inline headers only — so the token is read here and sent as Authorization. A server whose token
// does not resolve is DROPPED: the adapter would otherwise hold an unauthenticated tool the model
// spends its turn getting 401s from. A missing mcpFile, or one with no servers, is an empty list
// and no error, unlike the file generators: an agent that gets no MCP still runs.
func ACPServers(mcpFile string, lookupEnv func(string) (string, bool)) ([]map[string]any, error) {
	out := []map[string]any{}
	if mcpFile == "" {
		return out, nil
	}
	_, servers, present, err := loadServerViewsOptional(mcpFile)
	if err != nil {
		return nil, err
	}
	if !present {
		return out, nil
	}
	for _, name := range sortedKeys(servers) {
		if rendered := acpServer(name, servers[name], lookupEnv); rendered != nil {
			out = append(out, rendered)
		}
	}
	return out, nil
}

// acpServer renders one entry, or nil for one the adapter must not be given.
func acpServer(name string, s server, lookupEnv func(string) (string, bool)) map[string]any {
	switch {
	case s.URL != "":
		headers := make([]map[string]any, 0, len(s.Headers)+1)
		for _, key := range sortedKeys(s.Headers) {
			headers = append(headers, map[string]any{"name": key, "value": envValueString(s.Headers[key])})
		}
		if s.BearerTokenEnvVar != "" {
			token := ""
			if lookupEnv != nil {
				token, _ = lookupEnv(s.BearerTokenEnvVar)
			}
			if strings.TrimSpace(token) == "" {
				return nil // unauthenticatable — see ACPServers
			}
			headers = append(headers, map[string]any{"name": "Authorization", "value": "Bearer " + token})
		}
		// "type" is what tells the adapter this is a remote server at all; without it the entry
		// falls through to its stdio arm and becomes a server with no command.
		transport := "http"
		if s.Type == "sse" {
			transport = "sse"
		}
		rendered := map[string]any{"type": transport, "name": name, "url": s.URL}
		if len(headers) > 0 {
			rendered["headers"] = headers
		}
		return rendered
	case s.Command != "":
		// No "type" key: the adapter reads a stdio server by the ABSENCE of one, so declaring
		// the "stdio" that mcp.json is entitled to write would drop the server silently.
		rendered := map[string]any{"name": name, "command": s.Command}
		if len(s.Args) > 0 {
			rendered["args"] = s.Args
		}
		env := make([]map[string]any, 0, len(s.Env))
		for _, key := range sortedKeys(s.Env) {
			env = append(env, map[string]any{"name": key, "value": envValueString(s.Env[key])})
		}
		if len(env) > 0 {
			rendered["env"] = env
		}
		return rendered
	}
	// No transport — skip this malformed/empty entry, as writeCodexServer does.
	return nil
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
	if s.URL == "" && s.Command == "" {
		// No transport — skip this malformed/empty entry rather than emit a bodyless
		// [mcp_servers.<name>] table, which Codex may reject and so break ALL its MCP servers.
		return
	}
	fmt.Fprintf(b, "[mcp_servers.%s]\n", tomlKey(name))
	switch {
	case s.URL != "": // streamable HTTP server
		fmt.Fprintf(b, "url = %s\n", tomlString(s.URL))
		switch {
		case s.BearerTokenEnvVar != "":
			fmt.Fprintf(b, "bearer_token_env_var = %s\n", tomlString(s.BearerTokenEnvVar))
		case len(s.Headers) > 0:
			// Codex (unlike claude/gemini) has no inline-header support for HTTP MCP servers — only
			// bearer_token_env_var / OAuth (codex 0.141.0 `mcp add --help`). It can't use the
			// "headers" claude/gemini authenticate with, so flag the gap rather than emit a silent
			// unauthenticated url that 401s mid-run.
			b.WriteString("# coop: codex can't use this server's \"headers\" — set bearer_token_env_var to authenticate it\n")
		}
	case s.Command != "": // stdio server
		fmt.Fprintf(b, "command = %s\n", tomlString(s.Command))
		if len(s.Args) > 0 {
			fmt.Fprintf(b, "args = %s\n", tomlStringArray(s.Args))
		}
		if len(s.Env) > 0 {
			b.WriteString("\n")
			fmt.Fprintf(b, "[mcp_servers.%s.env]\n", tomlKey(name))
			for _, k := range sortedKeys(s.Env) {
				fmt.Fprintf(b, "%s = %s\n", tomlKey(k), tomlString(envValueString(s.Env[k])))
			}
		}
	}
}

// keepNonMCP returns the user's native TOML with its canonical bare [mcp_servers.*] tables
// removed. Every retained byte stays verbatim; alternate semantic spellings fail closed instead of
// surviving beside the generated authority. Only an initially absent path is an empty config.
func keepNonMCP(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := readNativeConfig(path)
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", nil
	}
	original, err := parseTOML(path, data)
	if err != nil {
		return "", err
	}
	if _, hasMCP := original["mcp_servers"]; !hasMCP {
		return string(data), nil
	}
	delete(original, "mcp_servers")
	kept := stripCanonicalMCP(data)
	remaining, err := parseTOML(path, kept)
	if err != nil {
		return "", unsupportedNativeMCP(path)
	}
	if _, stillPresent := remaining["mcp_servers"]; stillPresent || !tomlSemanticEqual(original, remaining) {
		return "", unsupportedNativeMCP(path)
	}
	return string(kept), nil
}

func readNativeConfig(path string) ([]byte, error) {
	data, _, err := readConfigFile(path, "native MCP config")
	return data, err
}

// readNativeConfigWith makes the observation/open/read boundary deterministic in tests. The
// production path opens once, validates that descriptor, and reads that same descriptor.
func readNativeConfigWith(path string, open func(string) (*os.File, error), read func(io.Reader) ([]byte, error)) ([]byte, error) {
	data, _, err := readConfigFileWith(path, "native MCP config", open, read)
	return data, err
}

// readConfigFile is the one host-file boundary for shared MCP and native adapter configuration.
// It does not follow the final symlink, never blocks on a special file, and bounds the exact bytes
// captured from the descriptor it validated. Only a path missing at the initial observation is
// inert; every other inspection, open, or read failure must stop projection.
func readConfigFile(path, kind string) ([]byte, bool, error) {
	return readConfigFileWith(path, kind, func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	}, io.ReadAll)
}

func readConfigFileWith(path, kind string, open func(string) (*os.File, error), read func(io.Reader) ([]byte, error)) ([]byte, bool, error) {
	initial, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect %s %s: %w", kind, path, err)
	}
	if initial.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("%s %s is a symbolic link", kind, path)
	}
	if !initial.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s %s is not a regular file", kind, path)
	}
	f, err := open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open %s %s: %w", kind, path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspect opened %s %s: %w", kind, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s %s is not a regular file", kind, path)
	}
	if info.Size() > maxMCPConfigBytes {
		return nil, false, fmt.Errorf("%s %s exceeds the %d-byte limit", kind, path, maxMCPConfigBytes)
	}
	data, err := read(io.LimitReader(f, maxMCPConfigBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %s %s: %w", kind, path, err)
	}
	if int64(len(data)) > maxMCPConfigBytes {
		return nil, false, fmt.Errorf("%s %s exceeds the %d-byte limit", kind, path, maxMCPConfigBytes)
	}
	return data, true, nil
}

func parseTOML(path string, data []byte) (map[string]any, error) {
	root := map[string]any{}
	if err := toml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("existing %s is not valid TOML: %w", path, err)
	}
	return root, nil
}

func stripCanonicalMCP(data []byte) []byte {
	var kept bytes.Buffer
	skip := false
	for len(data) > 0 {
		n := bytes.IndexByte(data, '\n')
		if n < 0 {
			n = len(data)
		} else {
			n++
		}
		line := data[:n]
		data = data[n:]
		if s := strings.TrimSpace(string(line)); strings.HasPrefix(s, "[") {
			skip = strings.HasPrefix(s, "[mcp_servers.") || strings.HasPrefix(s, "[mcp_servers]")
		}
		if !skip {
			kept.Write(line)
		}
	}
	return kept.Bytes()
}

func unsupportedNativeMCP(path string) error {
	return fmt.Errorf("native MCP config %s uses an mcp_servers form Coop cannot safely remove — move those servers to the active shared MCP file (COOP_MCP_FILE) and remove the native declaration", path)
}

func tomlSemanticEqual(a, b any) bool {
	return tomlValueEqual(reflect.ValueOf(a), reflect.ValueOf(b))
}

func tomlValueEqual(a, b reflect.Value) bool {
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	if a.Type() != b.Type() {
		return false
	}
	switch a.Kind() {
	case reflect.Interface:
		return tomlValueEqual(a.Elem(), b.Elem())
	case reflect.Map:
		if a.Len() != b.Len() {
			return false
		}
		for _, key := range a.MapKeys() {
			if other := b.MapIndex(key); !other.IsValid() || !tomlValueEqual(a.MapIndex(key), other) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		if a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !tomlValueEqual(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Float32, reflect.Float64:
		return a.Float() == b.Float() || (math.IsNaN(a.Float()) && math.IsNaN(b.Float()))
	default:
		return reflect.DeepEqual(a.Interface(), b.Interface())
	}
}

func loadServersAny(path string) (map[string]any, error) {
	servers, _, err := loadServerViews(path)
	if err != nil {
		return nil, err
	}
	return servers, nil
}

func loadServersTyped(path string) (map[string]server, error) {
	_, servers, err := loadServerViews(path)
	return servers, err
}

// ReadValidatedSnapshot captures the exact shared mcp.json bytes and validates the server views
// every adapter relies on. Missing files and files with no servers are inert; a present malformed
// or ambiguous authority fails closed before a box can mount or render it.
func ReadValidatedSnapshot(path string) ([]byte, bool, error) {
	if path == "" {
		return nil, false, nil
	}
	data, present, err := readConfigFile(path, "shared MCP config")
	if err != nil {
		return nil, false, err
	}
	if !present {
		return nil, false, nil
	}
	_, servers, err := loadServerViewsData(path, data)
	if err != nil {
		return nil, false, err
	}
	if len(servers) == 0 {
		return nil, false, nil
	}
	return data, true, nil
}

// loadServerViews decodes the shared authority once into both shapes its consumers need. Gemini
// gets the native JSON object while typed consumers get the projected servers, but every adapter
// crosses the same validation boundary before provider-specific rendering can interpret auth.
func loadServerViews(path string) (map[string]any, map[string]server, error) {
	raw, typed, present, err := loadServerViewsOptional(path)
	if err != nil {
		return nil, nil, err
	}
	if !present {
		return nil, nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	}
	return raw, typed, nil
}

func loadServerViewsOptional(path string) (map[string]any, map[string]server, bool, error) {
	data, present, err := readConfigFile(path, "shared MCP config")
	if err != nil {
		return nil, nil, false, err
	}
	if !present {
		return nil, nil, false, nil
	}
	raw, typed, err := loadServerViewsData(path, data)
	return raw, typed, true, err
}

func loadServerViewsData(path string, data []byte) (map[string]any, map[string]server, error) {
	if !utf8.Valid(data) {
		return nil, nil, fmt.Errorf("parsing %s: invalid UTF-8", path)
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for _, key := range sortedKeys(root) {
		if strings.EqualFold(key, "mcpServers") && key != "mcpServers" {
			return nil, nil, fmt.Errorf("parsing %s: MCP root key %q must use canonical spelling %q", path, key, "mcpServers")
		}
	}
	var definitions map[string]json.RawMessage
	if raw := root["mcpServers"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &definitions); err != nil {
			return nil, nil, fmt.Errorf("parsing %s mcpServers: %w", path, err)
		}
	}
	raw := make(map[string]any, len(definitions))
	typed := make(map[string]server, len(definitions))
	for _, name := range sortedKeys(definitions) {
		definition := definitions[name]
		if err := validateCanonicalServerFields(name, definition); err != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		var native any
		if err := json.Unmarshal(definition, &native); err != nil {
			return nil, nil, fmt.Errorf("parsing %s MCP server %q: %w", path, name, err)
		}
		var projected server
		if err := json.Unmarshal(definition, &projected); err != nil {
			return nil, nil, fmt.Errorf("parsing %s MCP server %q: %w", path, name, err)
		}
		raw[name] = native
		typed[name] = projected
	}
	if err := validateServers(typed); err != nil {
		return nil, nil, err
	}
	return raw, typed, nil
}

var canonicalServerFields = []string{
	"type", "command", "args", "env", "url", "headers", "bearer_token_env_var",
}

func validateCanonicalServerFields(name string, definition json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(definition, &fields); err != nil {
		return fmt.Errorf("MCP server %q: %w", name, err)
	}
	for _, key := range sortedKeys(fields) {
		for _, canonical := range canonicalServerFields {
			if strings.EqualFold(key, canonical) && key != canonical {
				return fmt.Errorf("MCP server %q field %q must use canonical spelling %q", name, key, canonical)
			}
		}
	}
	return nil
}

// rejectDuplicateJSONKeys refuses an ambiguity encoding/json would otherwise resolve by silently
// keeping the last value. Direct and translated consumers must see one authority, including inside
// nested header and environment objects.
func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkUniqueJSONValue(decoder, "root"); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func walkUniqueJSONValue(decoder *json.Decoder, location string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("non-string JSON object key at %s", location)
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON object key %q at %s", key, location)
			}
			seen[key] = true
			if err := walkUniqueJSONValue(decoder, location+"."+key); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := walkUniqueJSONValue(decoder, fmt.Sprintf("%s[%d]", location, index)); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delim, location)
	}
}

func validateServers(servers map[string]server) error {
	for _, name := range sortedKeys(servers) {
		s := servers[name]
		seen := make(map[string]string, len(s.Headers))
		for _, header := range sortedKeys(s.Headers) {
			folded := strings.ToLower(header)
			if prior, ok := seen[folded]; ok {
				return fmt.Errorf("MCP server %q declares duplicate case-insensitive headers %q and %q", name, prior, header)
			}
			seen[folded] = header
			if s.BearerTokenEnvVar != "" && strings.EqualFold(header, "Authorization") {
				return fmt.Errorf("MCP server %q declares both %q and bearer_token_env_var; choose one Authorization source", name, header)
			}
		}
	}
	return nil
}

// readJSONObject reads a JSON object from path. A missing or empty file (or "") yields an empty
// object — there's nothing to merge onto. Every other read failure and a present-but-malformed file
// are errors rather than excuses to overwrite the user's settings with generated defaults.
func readJSONObject(path string) (map[string]any, error) {
	out := map[string]any{}
	if path == "" {
		return out, nil
	}
	data, present, err := readConfigFile(path, "native MCP config")
	if err != nil {
		return nil, err
	}
	if !present {
		return out, nil // no existing settings → start fresh
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return out, nil // empty file → start fresh
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("existing %s is not valid JSON: %w", path, err)
	}
	return out, nil
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
	// Escape the control characters a TOML basic string forbids (a raw \n/\t in an env value would
	// otherwise produce invalid TOML and break the whole config), plus \ and ".
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s) + `"`
}

// tomlKey renders a TOML table-name segment / bare key, quoting it when it isn't a bare key
// (^[A-Za-z0-9_-]+$). A server name with a dot would otherwise NEST the table (my.server →
// mcp_servers.my.server, a server named "server" under "my"), and one with a space would be
// invalid TOML and break every server — so coop quotes them.
func tomlKey(s string) string {
	bare := s != ""
	for _, r := range s {
		if !(r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			bare = false
			break
		}
	}
	if bare {
		return s
	}
	return tomlString(s)
}

func tomlStringArray(a []string) string {
	parts := make([]string, len(a))
	for i, x := range a {
		parts[i] = tomlString(x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
