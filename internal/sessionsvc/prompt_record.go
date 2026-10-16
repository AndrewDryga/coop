package sessionsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/AndrewDryga/coop/internal/session"
)

// What one session/prompt carried, recorded at the send (session.EventPromptSent).
//
// The caller holds its own prompt and output schema, so those are recorded as
// digests where the text carries them; every byte Coop wrote around them, the
// output contract, a correction and the output-folder note, is recorded as
// sent. The digest of the whole text lets the caller prove that the copy it
// rebuilds from its own prompt is the text the model was given.
const (
	promptRecordVersion = 1
	// Coop's own text in one send is a few KiB: the contract wrapper and the
	// folder note are under 2 KiB and a correction's error detail is bounded.
	// A larger part is recorded by its digest and size only, so the record
	// never ships a second copy of a caller's prompt.
	maximumPromptRecordPartBytes = 16 << 10
	maximumPromptRecordTextBytes = 32 << 10
)

type promptRecordPart struct {
	Kind   string `json:"kind"`
	Text   string `json:"text,omitempty"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

type promptRecordBlock struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Bytes     int    `json:"bytes"`
}

// recordPromptSent appends the record of one send. It is evidence, not
// custody: a record that cannot be written leaves the turn running.
func (r *sessionTurnRunner) recordPromptSent(
	ctx context.Context,
	sessionID string,
	leased session.Turn,
	content []map[string]any,
	sendErr error,
) {
	if r.store == nil || len(content) == 0 {
		return
	}
	payload, err := promptRecordPayload(content, leased, sendErr, ctx.Err() != nil)
	if err != nil || len(payload) > session.MaxEventPayloadBytes {
		return
	}
	appendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionACPCleanupTimeout)
	defer cancel()
	_, _ = r.store.AppendEvent(appendCtx, session.AppendEventRequest{
		SessionID: sessionID,
		TurnID:    leased.ID,
		Type:      session.EventPromptSent,
		Version:   1,
		Payload:   payload,
	})
}

// A write that failed after cancellation may still have reached the adapter,
// so it is uncertain rather than failed.
func promptRecordPayload(
	content []map[string]any,
	leased session.Turn,
	sendErr error,
	cancelled bool,
) ([]byte, error) {
	state := "sent"
	switch {
	case sendErr == nil:
	case cancelled:
		state = "uncertain"
	default:
		state = "failed"
	}
	text, _ := content[0]["text"].(string)
	return json.Marshal(map[string]any{
		"version":     promptRecordVersion,
		"state":       state,
		"text_sha256": sha256Hex(text),
		"text_bytes":  len(text),
		"parts":       promptRecordParts(text, leased.Prompt, leased.OutputContract),
		"blocks":      promptRecordBlocks(leased.Artifacts, content),
	})
}

// promptRecordParts splits the text into the caller's prompt where it opens
// the text, each copy of the output schema inside a <json-schema> element, and
// the text between them, in order. Their concatenation is the text.
func promptRecordParts(text, prompt string, contract *session.OutputContract) []promptRecordPart {
	parts := []promptRecordPart{}
	rest := text
	if prompt != "" && strings.HasPrefix(rest, prompt) {
		parts = append(parts, promptRecordDigest("caller_prompt", prompt))
		rest = rest[len(prompt):]
	}
	if contract != nil && len(contract.JSONSchema) > 0 {
		schema := string(contract.JSONSchema)
		element := "<json-schema>" + schema + "</json-schema>"
		for {
			index := strings.Index(rest, element)
			if index < 0 {
				break
			}
			opening := index + len("<json-schema>")
			parts = appendPromptRecordText(parts, rest[:opening])
			parts = append(parts, promptRecordDigest("output_schema", schema))
			rest = rest[opening+len(schema):]
		}
	}
	return boundPromptRecordText(appendPromptRecordText(parts, rest))
}

func appendPromptRecordText(parts []promptRecordPart, text string) []promptRecordPart {
	if text == "" {
		return parts
	}
	if len(text) > maximumPromptRecordPartBytes {
		return append(parts, promptRecordDigest("omitted", text))
	}
	return append(parts, promptRecordPart{Kind: "text", Text: text, SHA256: sha256Hex(text), Bytes: len(text)})
}

// boundPromptRecordText keeps the recorded text inside its bound, recording
// the largest parts by digest first.
func boundPromptRecordText(parts []promptRecordPart) []promptRecordPart {
	total := 0
	for _, part := range parts {
		total += len(part.Text)
	}
	if total <= maximumPromptRecordTextBytes {
		return parts
	}
	order := make([]int, len(parts))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(a, b int) bool { return len(parts[order[a]].Text) > len(parts[order[b]].Text) })
	for _, index := range order {
		if total <= maximumPromptRecordTextBytes {
			break
		}
		total -= len(parts[index].Text)
		parts[index] = promptRecordPart{Kind: "omitted", SHA256: parts[index].SHA256, Bytes: parts[index].Bytes}
	}
	return parts
}

// Each attachment travels as a content block of its own after the text
// (artifact.go); its identity is recorded, never its bytes.
func promptRecordBlocks(artifacts []session.InputArtifact, content []map[string]any) []promptRecordBlock {
	blocks := make([]promptRecordBlock, 0, len(artifacts))
	for index, artifact := range artifacts {
		kind := ""
		if index+1 < len(content) {
			kind, _ = content[index+1]["type"].(string)
		}
		blocks = append(blocks, promptRecordBlock{
			Type: kind, Name: artifact.Name, MediaType: artifact.MediaType,
			SHA256: artifact.SHA256, Bytes: len(artifact.Data),
		})
	}
	return blocks
}

func promptRecordDigest(kind, text string) promptRecordPart {
	return promptRecordPart{Kind: kind, SHA256: sha256Hex(text), Bytes: len(text)}
}

func sha256Hex(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}
