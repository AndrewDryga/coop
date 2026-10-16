package workerconnector

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// The record of one session/prompt (sessionsvc/prompt_record.go) crosses to
// the controller so that it can show the text the model was given: its own
// prompt and output schema by digest, and Coop's text as sent. A text part
// crosses exactly or not at all: one that scans as carrying a likely secret
// (a correction can quote the answer it rejects) or that names the checkout
// root crosses as its digest and size, marked withheld with the reason.
const (
	maximumPromptRecordParts      = 16
	maximumPromptRecordBlocks     = 16
	maximumPromptRecordPartBytes  = 16 << 10
	maximumPromptRecordTotalBytes = 32 << 10
)

func copyPromptRecord(public map[string]any, value map[string]any, root string) bool {
	if version, ok := value["version"].(float64); !ok || version != 1 {
		return false
	}
	public["version"] = int64(1)
	copyEnum(public, "state", value["state"], "sent", "failed", "uncertain")
	copyDigest(public, "text_sha256", value["text_sha256"])
	copyPublicInteger(public, "text_bytes", value["text_bytes"])
	if public["state"] == nil || public["text_sha256"] == nil || public["text_bytes"] == nil {
		return false
	}
	rawParts, ok := value["parts"].([]any)
	if !ok || len(rawParts) > maximumPromptRecordParts {
		return false
	}
	parts := make([]any, 0, len(rawParts))
	total := 0
	for _, raw := range rawParts {
		part, ok := promptRecordPart(raw, root, &total)
		if !ok {
			return false
		}
		parts = append(parts, part)
	}
	public["parts"] = parts
	rawBlocks, ok := value["blocks"].([]any)
	if !ok || len(rawBlocks) > maximumPromptRecordBlocks {
		return false
	}
	blocks := make([]any, 0, len(rawBlocks))
	for _, raw := range rawBlocks {
		block, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		copied := map[string]any{}
		copyEnum(copied, "type", block["type"], "image", "text", "resource")
		copyPublicText(copied, "name", block["name"], 256)
		copyPublicText(copied, "media_type", block["media_type"], 256)
		copyDigest(copied, "sha256", block["sha256"])
		copyPublicInteger(copied, "bytes", block["bytes"])
		blocks = append(blocks, copied)
	}
	public["blocks"] = blocks
	return true
}

func promptRecordPart(raw any, root string, total *int) (map[string]any, bool) {
	part, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	kind, _ := part["kind"].(string)
	copied := map[string]any{"kind": kind}
	copyDigest(copied, "sha256", part["sha256"])
	copyPublicInteger(copied, "bytes", part["bytes"])
	if copied["sha256"] == nil || copied["bytes"] == nil {
		return nil, false
	}
	switch kind {
	case "caller_prompt", "output_schema", "omitted":
		return copied, true
	case "text":
	default:
		return nil, false
	}
	text, ok := part["text"].(string)
	if !ok || text == "" || sha256Text(text) != copied["sha256"] {
		return nil, false
	}
	reason := secretReason(text)
	switch {
	case reason != "":
	case root != "" && strings.Contains(text, root):
		reason = "names the checkout root"
	case len(text) > maximumPromptRecordPartBytes || *total+len(text) > maximumPromptRecordTotalBytes:
		reason = "too large"
	}
	if reason != "" {
		copied["kind"] = "withheld"
		copied["reason"] = reason
		return copied, true
	}
	*total += len(text)
	copied["text"] = text
	return copied, true
}

func copyDigest(target map[string]any, key string, raw any) {
	value, ok := raw.(string)
	if !ok || len(value) != 64 {
		return
	}
	if _, err := hex.DecodeString(value); err == nil && strings.ToLower(value) == value {
		target[key] = value
	}
}

func sha256Text(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}
