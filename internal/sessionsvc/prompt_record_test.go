package sessionsvc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
		var carried struct {
			CheckoutRoot string `json:"checkout_root"`
		}
		if json.Unmarshal(payload, &carried) != nil || carried.CheckoutRoot != fixture.session.Workspace {
			t.Fatalf("send %d record lost the worker's checkout root: %s", index+1, payload)
		}
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

// The worker withholds a prompt text part that names the host checkout, but it learns the root
// only from the event: prompt.sent once carried none, so that guard never fired. The record now
// carries it like narrated activity does. testdata/prompt_sent.json pins this output; the worker's
// own test reads the same file (workerconnector/prompt_record_test.go).
func TestPromptRecordCarriesTheCheckoutRootForTheWorker(t *testing.T) {
	const root = "/home/dev/shop"
	leased := session.Turn{Prompt: "Fix the build"}
	text := leased.Prompt + "\n\n<coop-output>Save only final generated images in " + root + "/.coop-output/turn-1.</coop-output>"
	payload, err := promptRecordPayload([]map[string]any{{"type": "text", "text": text}}, leased, root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "prompt_sent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(payload) + "\n"; got != string(want) {
		t.Fatalf("the daemon's prompt record changed; update testdata/prompt_sent.json and run the worker's test against it:\n%s", got)
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

// A schema repair resent the turn's images and resources, up to 8 MiB a round, into the native
// session that already held them. The correction reloads that same session, so it now carries
// only text, and its record lists only what it carried.
func TestSchemaRepairSendsTheTurnImagesOnlyOnce(t *testing.T) {
	fixture := newSessionACPFixture(t, "invalid-contract-once")
	image := []byte("\x89PNG\r\n\x1a\nimage")
	note := []byte("plain notes\n")
	schema := json.RawMessage(`{"type":"object","properties":{"reply":{"type":"string"}},"required":["reply"],"additionalProperties":false}`)
	leased := fixture.submitRequest(t, session.SubmitTurnRequest{
		Prompt:         "inspect the screenshot",
		OutputContract: &session.OutputContract{JSONSchema: schema, SHA256: sha256Hex(string(schema))},
		Artifacts: []session.InputArtifact{
			{Name: "bug.png", MediaType: "image/png", SHA256: sha256Hex(string(image)), Data: image},
			{Name: "notes.txt", MediaType: "text/plain", SHA256: sha256Hex(string(note)), Data: note},
		},
	})
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, leased); err != nil {
		t.Fatal(err)
	}
	var sends [][]map[string]any
	var loads int
	for line := range strings.Lines(readFile(t, fixture.childLog)) {
		var frame struct {
			Method string `json:"method"`
			Params struct {
				Prompt []map[string]any `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &frame) != nil {
			continue
		}
		switch frame.Method {
		case "session/prompt":
			sends = append(sends, frame.Params.Prompt)
		case "session/load":
			loads++
		}
	}
	kinds := func(prompt []map[string]any) []string {
		var kinds []string
		for _, block := range prompt {
			kind, _ := block["type"].(string)
			kinds = append(kinds, kind)
		}
		return kinds
	}
	if len(sends) != 2 || loads != 1 {
		t.Fatalf("%d sends and %d session/load, want the first prompt and a correction in the reloaded session", len(sends), loads)
	}
	if got := strings.Join(kinds(sends[0]), ","); got != "text,image,text" {
		t.Fatalf("first delivery blocks = %s, want the image and the text attachment note", got)
	}
	if got := strings.Join(kinds(sends[1]), ","); got != "text,text" {
		t.Fatalf("correction blocks = %s, want text only, the attachment note kept", got)
	}
	records := promptSentRecords(t, fixture.store, fixture.session.ID)
	if len(records) != 2 {
		t.Fatalf("%d prompt records", len(records))
	}
	for index, want := range []string{"image,text", "text"} {
		var record struct {
			Blocks []promptRecordBlock `json:"blocks"`
		}
		if err := json.Unmarshal(records[index], &record); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, block := range record.Blocks {
			got = append(got, block.Type)
		}
		if strings.Join(got, ",") != want {
			t.Fatalf("record %d blocks = %v, want %s", index, got, want)
		}
	}
}
