package workerconnector

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/secretscan"
)

// What a worker narrates to its controller about the inside of a turn: the
// command a tool ran and what it printed, a tool's title, the model's thoughts
// and progress, and its plan. Ryker's timeline showed "Run command — the
// worker reported that a command ran, not which one" for every shell step,
// and nothing of what the model was thinking (Andrew, 2026-09-28: "workers
// must report commands, thinking and everything else").
//
// Each field crosses bounded, with the session's checkout root taken out of it,
// and whole or not at all: a field that scans as carrying a likely secret is
// withheld and the event says which field and why, so the controller can say
// so instead of showing nothing. A field larger than its bound crosses as a
// marked preview of its text.
const (
	maximumNarrationTitleBytes = 1 << 10
	maximumNarrationTextBytes  = 8 << 10
	maximumNarrationFieldBytes = 16 << 10
	// One event's narration, as encoded. A poll carries at most 512 KiB of
	// events and stops at the first that does not fit, so no event may come
	// near it.
	maximumNarrationEventBytes = 64 << 10
)

// narration copies the narrated fields of one activity event into public.
type narration struct {
	public   map[string]any
	root     string
	withheld map[string]any
}

// The daemon puts the session's checkout root on every narrated event (sessionsvc/activity.go); it
// is read here and never copied out.
func newNarration(public map[string]any, value map[string]any) *narration {
	root := ""
	if text, ok := value["checkout_root"].(string); ok && len(text) > 1 {
		root = strings.TrimRight(text, "/")
	}
	return &narration{public: public, root: root, withheld: map[string]any{}}
}

// text copies one free-form text field.
func (n *narration) text(key string, raw any, maximum int) {
	text, ok := raw.(string)
	if !ok || strings.TrimSpace(text) == "" || strings.ContainsRune(text, 0) {
		return
	}
	text = n.withoutRoot(strings.ToValidUTF8(text, "�"))
	if reason := secretReason(text); reason != "" {
		n.withheld[key] = reason
		return
	}
	n.public[key] = utf8Prefix(text, maximum)
}

// evidence copies one structured tool field: the value itself when it is small,
// a marked preview of its text when it is not.
func (n *narration) evidence(key string, raw any) {
	if raw == nil {
		return
	}
	encoded, err := narrationJSON(raw)
	if err != nil {
		n.withheld[key] = "not valid JSON"
		return
	}
	text := n.withoutRoot(strings.ToValidUTF8(string(encoded), "�"))
	if reason := secretReason(text); reason != "" {
		n.withheld[key] = reason
		return
	}
	if len(text) <= maximumNarrationFieldBytes {
		var value any
		if json.Unmarshal([]byte(text), &value) == nil {
			n.public[key] = value
			return
		}
	}
	n.public[key] = map[string]any{"preview": utf8Prefix(text, maximumNarrationFieldBytes), "truncated": true}
}

// finish keeps the event inside its bound, withholding its largest fields
// first (an input keeps the names of its tool), then records what was withheld.
func (n *narration) finish() {
	for _, key := range []string{"output", "content", "locations", "input", "text", "title"} {
		if encoded, err := narrationJSON(n.public); err == nil && len(encoded) <= maximumNarrationEventBytes {
			break
		}
		value, ok := n.public[key]
		if !ok {
			continue
		}
		delete(n.public, key)
		n.withheld[key] = "too large"
		if input, ok := value.(map[string]any); ok && key == "input" {
			names := map[string]any{}
			for _, name := range []string{"server", "tool", "operation"} {
				if text, ok := input[name].(string); ok {
					names[name] = text
				}
			}
			if len(names) > 0 {
				n.public["input"] = names
			}
		}
	}
	if len(n.withheld) > 0 {
		n.public["withheld"] = n.withheld
	}
}

// The session's checkout root stays on the worker: a path under it crosses
// relative to it.
func (n *narration) withoutRoot(text string) string {
	if n.root == "" {
		return text
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, n.root+"/", ""), n.root, ".")
}

func secretReason(text string) string {
	if findings := secretscan.ScanSecrets(text); len(findings) > 0 {
		return "likely " + findings[0].Kind
	}
	return ""
}

func narrationJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

func utf8Prefix(text string, maximum int) string {
	if len(text) <= maximum {
		return text
	}
	cut := maximum
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
