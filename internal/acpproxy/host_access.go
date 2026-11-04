package acpproxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// An editor can offer an agent its own computer: fs/read_text_file, fs/write_text_file and terminal/*
// run on the host, outside the box, and a login button can run a command there. A filtered session
// promises that nothing leaves the box except by its rules, and any program in the box can write
// straight onto the adapter's stdout. So with RunOpts.NoEditorHostAccess the adapter is not told the
// editor can do this, the login buttons stay hidden, and a box may ask the editor only for what is
// listed here or is a question in a form. Any other method is refused, including one ACP adds later.
var boxMayAskEditor = map[string]bool{"session/update": true, "session/request_permission": true}

// jsonRPCKeys are the top-level keys an editor acts on.
var jsonRPCKeys = []string{"jsonrpc", "id", "method", "params", "result", "error"}

// withoutEditorHostAccess removes what the editor offers beyond the thread from its initialize: files,
// terminals and opening a link the box chose. A form stays, because it only asks the person something;
// Claude's question tool needs it. A line that does not parse goes on as it is: no adapter could read
// a capability from it either.
func withoutEditorHostAccess(line []byte) []byte {
	var message, params, capabilities, elicitation map[string]json.RawMessage
	if json.Unmarshal(line, &message) != nil || json.Unmarshal(message["params"], &params) != nil ||
		json.Unmarshal(params["clientCapabilities"], &capabilities) != nil {
		return line
	}
	delete(capabilities, "fs")
	delete(capabilities, "terminal")
	var err error
	if json.Unmarshal(capabilities["elicitation"], &elicitation) == nil && elicitation != nil {
		delete(elicitation, "url")
		if capabilities["elicitation"], err = json.Marshal(elicitation); err != nil {
			return line
		}
	}
	if params["clientCapabilities"], err = json.Marshal(capabilities); err != nil {
		return line
	}
	if message["params"], err = json.Marshal(params); err != nil {
		return line
	}
	out, err := json.Marshal(message)
	if err != nil {
		return line
	}
	return append(out, '\n')
}

// refuseEditorHostAccess reports whether a line from a box must not reach the editor, and answers a
// request so the adapter does not wait on it.
func refuseEditorHostAccess(child *Child, line []byte) bool {
	method, members, plain := readBoxLine(line)
	if !plain {
		Trace("refused a line from the box that a parser could read another way")
		return true
	}
	if method == "" || boxMayAskEditor[method] || method == "elicitation/create" && formQuestion(members["params"]) {
		return false
	}
	Trace("refused %q from the box: a filtered session does not reach the host through the editor", method)
	if id := members["id"]; id != nil {
		reply := `{"jsonrpc":"2.0","id":` + string(id) + `,"error":{"code":-32601,"message":` +
			`"A filtered box cannot ask the editor for this. Use the files and commands inside the box."}}` + "\n"
		_, _ = child.In.Write([]byte(reply))
	}
	return true
}

// formQuestion reports a question for the person, which is all a form can do. The other mode opens a
// URL the box chose.
func formQuestion(params json.RawMessage) bool {
	members, plain := plainObject(params, "mode")
	var mode string
	return plain && json.Unmarshal(members["mode"], &mode) == nil && mode == "form"
}

// readBoxLine reads what decides where a line from a box may go. plain is false for a line that readers
// can take differently, so that what is checked here is what the editor acts on: anything but one JSON
// object on one line (a carriage return ends a line for some readers, and is only whitespace to JSON),
// or an object plainObject refuses. An adapter sends none of these.
func readBoxLine(line []byte) (method string, members map[string]json.RawMessage, plain bool) {
	body := bytes.TrimRight(line, "\r\n")
	if bytes.IndexByte(body, '\r') >= 0 || !json.Valid(body) {
		return "", nil, false
	}
	members, plain = plainObject(body, jsonRPCKeys...)
	if !plain {
		return "", nil, false
	}
	if raw, named := members["method"]; named && json.Unmarshal(raw, &method) != nil {
		return "", nil, false
	}
	return method, members, true
}

// plainObject reads a JSON object's members. It reports false for one that parsers read differently: a
// repeated key (they keep the first, the last, or fail) or a named key in another spelling (Go's parser
// folds case, an editor's may not).
func plainObject(raw []byte, named ...string) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		return nil, false
	}
	members := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		var value json.RawMessage
		if err != nil || decoder.Decode(&value) != nil {
			return nil, false
		}
		key, _ := token.(string)
		if _, repeated := members[key]; repeated {
			return nil, false
		}
		for _, name := range named {
			if key != name && strings.EqualFold(key, name) {
				return nil, false
			}
		}
		members[key] = value
	}
	return members, true
}

// forEditor is the last word on a box's line before the editor reads it. The control layer hides the
// login buttons for its own reasons on some paths; a session without host access hides them on all.
func (p *proxy) forEditor(line []byte) []byte {
	if p.noHostAccess {
		return HideAuthenticationMethods(line)
	}
	return line
}

// HideAuthenticationMethods removes provider-owned login actions from an initialize answer. Provider
// switches negotiate fresh capabilities internally, but ACP has no notification that can replace the
// editor's original auth menu; leaving it visible produces a stale generic Authenticate button for the
// wrong provider. Coop's Account selector and login command own it. Keys may be written with JSON
// escapes, so the line is decoded rather than searched.
func HideAuthenticationMethods(line []byte) []byte {
	var message, result map[string]json.RawMessage
	if json.Unmarshal(line, &message) != nil || json.Unmarshal(message["result"], &result) != nil {
		return line
	}
	if _, named := result["authMethods"]; !named && len(result["protocolVersion"]) == 0 {
		return line
	}
	result["authMethods"] = json.RawMessage("[]")
	if raw := result["agentCapabilities"]; len(raw) > 0 {
		var capabilities map[string]json.RawMessage
		if json.Unmarshal(raw, &capabilities) == nil {
			delete(capabilities, "auth") // newer ACP versions advertise logout here
			if encodedCapabilities, err := json.Marshal(capabilities); err == nil {
				result["agentCapabilities"] = encodedCapabilities
			}
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return line
	}
	message["result"] = encoded
	encoded, err = json.Marshal(message)
	if err != nil {
		return line
	}
	return append(encoded, '\n')
}
