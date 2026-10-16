package sessionsvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
)

// Ryker showed its own saved prompt as the prompt a model was given, but the
// worker sent more: the output contract after it, the output-folder note, and
// a correction on its own when the first answer failed the schema
// (2026-10-15). Every send now leaves a record from which the caller rebuilds
// the exact text out of its own prompt and schema.
func TestEverySentPromptIsRecordedSoTheCallerCanRebuildItExactly(t *testing.T) {
	fixture := newSessionACPFixture(t, "invalid-contract-once")
	leased := fixture.submitContract(t, "Read the alert — «ünïcode» stays exact ✓")
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, leased); err != nil {
		t.Fatal(err)
	}

	wire := sentPromptTexts(t, fixture.childLog)
	records := promptSentRecords(t, fixture.store, fixture.session.ID)
	if len(wire) != 2 || len(records) != len(wire) {
		t.Fatalf("%d records for %d sends, want one for the first prompt and one for the correction", len(records), len(wire))
	}
	for index, payload := range records {
		rebuilt, record := rebuildSentPrompt(t, payload, leased.Prompt, leased.OutputContract.JSONSchema)
		if rebuilt != wire[index] {
			t.Fatalf("send %d rebuilt as\n%q\nwant the text sent\n%q", index+1, rebuilt, wire[index])
		}
		if record.State != "sent" || record.TextSHA256 != sha256Hex(wire[index]) || record.TextBytes != len(wire[index]) {
			t.Fatalf("send %d record = %+v", index+1, record)
		}
	}
	first, _ := rebuildSentPrompt(t, records[0], leased.Prompt, leased.OutputContract.JSONSchema)
	if !strings.HasPrefix(first, leased.Prompt) || !strings.Contains(first, "<coop-output>") {
		t.Fatalf("the first send lost the caller's prompt or the folder note: %q", first)
	}
	if strings.Contains(string(records[1]), `"caller_prompt"`) ||
		!strings.Contains(wire[1], "This is correction attempt 2") {
		t.Fatalf("the correction is not recorded as Coop's own text: %s", records[1])
	}
	for _, payload := range records {
		_, record := rebuildSentPrompt(t, payload, leased.Prompt, leased.OutputContract.JSONSchema)
		for _, part := range record.Parts {
			if strings.Contains(part.Text, "ünïcode") || strings.Contains(part.Text, `"additionalProperties":false`) {
				t.Fatalf("the record carries a second copy of the caller's prompt or schema: %s", payload)
			}
		}
	}
}

// When the caller's prompt is not where the record expects it, the text is
// kept by digest and size rather than shipped as a copy of the prompt.
func TestPromptRecordKeepsAnUnexpectedlyLargeTextByDigestOnly(t *testing.T) {
	text := strings.Repeat("caller text ", 4_000) + "\n\n<coop-output>note</coop-output>"
	parts := promptRecordParts(text, "a prompt the text does not start with", nil)
	if len(parts) != 1 || parts[0].Kind != "omitted" || parts[0].Text != "" ||
		parts[0].SHA256 != sha256Hex(text) || parts[0].Bytes != len(text) {
		t.Fatalf("parts = %+v", parts)
	}
}

type sentPromptRecord struct {
	State      string             `json:"state"`
	TextSHA256 string             `json:"text_sha256"`
	TextBytes  int                `json:"text_bytes"`
	Parts      []promptRecordPart `json:"parts"`
}

func rebuildSentPrompt(t *testing.T, payload []byte, prompt string, schema []byte) (string, sentPromptRecord) {
	t.Helper()
	var record sentPromptRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, part := range record.Parts {
		switch part.Kind {
		case "caller_prompt":
			if part.SHA256 != sha256Hex(prompt) {
				t.Fatalf("caller prompt digest = %s", part.SHA256)
			}
			text.WriteString(prompt)
		case "output_schema":
			if part.SHA256 != sha256Hex(string(schema)) {
				t.Fatalf("schema digest = %s", part.SHA256)
			}
			text.Write(schema)
		case "text":
			text.WriteString(part.Text)
		default:
			t.Fatalf("part %q cannot be rebuilt", part.Kind)
		}
	}
	return text.String(), record
}

func promptSentRecords(t *testing.T, store *session.Store, sessionID string) [][]byte {
	t.Helper()
	events, err := store.ListEvents(context.Background(), sessionID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	records := [][]byte{}
	for _, event := range events {
		if event.Type == session.EventPromptSent {
			records = append(records, event.Payload)
		}
	}
	return records
}

// sentPromptTexts returns the text of every session/prompt request the child logged, in order.
func sentPromptTexts(t *testing.T, childLog string) []string {
	t.Helper()
	texts := []string{}
	for _, line := range strings.Split(strings.TrimSpace(readFile(t, childLog)), "\n") {
		var frame struct {
			Method string `json:"method"`
			Params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &frame) == nil && frame.Method == "session/prompt" && len(frame.Params.Prompt) > 0 {
			texts = append(texts, frame.Params.Prompt[0].Text)
		}
	}
	return texts
}
