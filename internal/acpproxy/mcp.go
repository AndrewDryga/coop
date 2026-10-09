package acpproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

var errSessionMCPHandoff = errors.New("could not configure this session's shared tools; reconnect the Coop editor connection")

// projectSessionMCP combines editor-owned servers with the current child's shared projection.
// Stored editor params stay unchanged so a replacement cannot inherit old broker stand-ins.
func projectSessionMCP(child *Child, line []byte) ([]byte, error) {
	h := parse(line)
	if child == nil || child.MCPServers == nil || !h.isRequest() {
		return line, nil
	}
	if !isSessionSetup(h.Method) {
		return line, nil
	}
	shared, err := child.MCPServers()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSessionMCPHandoff, err)
	}
	if len(shared) == 0 {
		return line, nil
	}
	var frame, params map[string]json.RawMessage
	if json.Unmarshal(line, &frame) != nil || json.Unmarshal(h.Params, &params) != nil || params == nil {
		return nil, errors.New("shared tools require session setup parameters")
	}
	var servers []json.RawMessage
	if raw, ok := params["mcpServers"]; ok {
		if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &servers) != nil {
			return nil, errors.New("session mcpServers must be an array")
		}
	}
	for _, server := range shared {
		raw, err := json.Marshal(server)
		if err != nil {
			return nil, errors.New("the shared tool projection is invalid")
		}
		servers = append(servers, raw)
	}
	seen := map[string][]byte{}
	merged := make([]json.RawMessage, 0, len(servers))
	for _, raw := range servers {
		var server map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&server) != nil {
			return nil, errors.New("a session MCP server is not an object")
		}
		name, _ := server["name"].(string)
		if name == "" {
			return nil, errors.New("a session MCP server has no name")
		}
		canonical, err := json.Marshal(server)
		if err != nil {
			return nil, errors.New("a session MCP server is invalid")
		}
		if prior, ok := seen[name]; ok {
			if !bytes.Equal(prior, canonical) {
				return nil, fmt.Errorf("MCP server %q has conflicting definitions; use unique names in editor and Coop shared configuration, then reconnect", name)
			}
			continue
		}
		seen[name] = canonical
		merged = append(merged, raw)
	}
	params["mcpServers"], err = json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	frame["params"], err = json.Marshal(params)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	Trace("shared MCP projection for %s: %d server(s)", h.Method, len(merged))
	return append(out, '\n'), nil
}

func isSessionSetup(method string) bool {
	return method == "session/new" || method == "session/load" || method == "session/resume"
}

func sessionMCPErrorResponse(id json.RawMessage, err error) []byte {
	code := -32602 // invalid editor setup parameters
	if errors.Is(err, errSessionMCPHandoff) {
		code = -32603 // the child could not supply its validated tool configuration
	}
	line, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": err.Error()},
	})
	return append(line, '\n')
}
