package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOutputContractUnicodeDiagnostics(t *testing.T) {
	// jsonschema reaches x/text through its diagnostic printer. Keep that real
	// application seam covered without adding an otherwise unused norm import.
	schema := json.RawMessage(`{"type":"object","properties":{"réponse":{"type":"string","enum":["はい","いいえ"]}},"required":["réponse"],"additionalProperties":false}`)
	digest := sha256.Sum256(schema)
	validator, err := CompileOutputContract(&OutputContract{
		JSONSchema: schema, SHA256: hex.EncodeToString(digest[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{`{"réponse":"はい"}`, `{"r\u00e9ponse":"いいえ"}`} {
		if err := validator.Validate([]byte(data)); err != nil {
			t.Fatalf("valid Unicode response %s: %v", data, err)
		}
	}
	for _, data := range []string{
		`{}`, `{"réponse":7}`, `{"réponse":"maybe"}`,
		// JSON property identity is exact, not Unicode-normalized.
		`{"re\u0301ponse":"はい"}`,
	} {
		err := validator.Validate([]byte(data))
		if err == nil {
			t.Fatalf("invalid Unicode response accepted: %s", data)
		}
		detail := err.Error()
		if !utf8.ValidString(detail) || !strings.Contains(detail, "réponse") ||
			!strings.Contains(detail, "final response does not match the output contract") {
			t.Fatalf("diagnostic lost the failing property/context for %s: %q", data, detail)
		}
	}
}

func TestOutputContractRejectsAmbiguousJSON(t *testing.T) {
	contract := func(schema string) *OutputContract {
		data := json.RawMessage(schema)
		digest := sha256.Sum256(data)
		return &OutputContract{JSONSchema: data, SHA256: hex.EncodeToString(digest[:])}
	}
	for _, schema := range []string{
		`{"type":"string","type":"number"}`, `{"properties":{"answer":{"type":"string","type":"number"}}}`,
		`{"properties":{"answer":true,"\u0061nswer":false}}`, `true false`,
	} {
		if _, err := CompileOutputContract(contract(schema)); CodeOf(err) != CodeInvalidRequest {
			t.Fatalf("ambiguous schema accepted: %s %v", schema, err)
		}
	}
	validator, err := CompileOutputContract(contract(`true`))
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{`{"answer":1,"answer":2}`, `{"outer":{"answer":1,"answer":2}}`, `[{"answer":1,"\u0061nswer":2}]`, `{} []`} {
		if err := validator.Validate([]byte(data)); err == nil || !strings.Contains(err.Error(), "unique-property JSON value") {
			t.Fatalf("ambiguous response accepted: %s %v", data, err)
		}
	}
	for _, data := range []string{`{"a":{"answer":1},"b":{"answer":2}}`, `[{"answer":1},{"answer":2}]`, "{}\n\t"} {
		if err := validator.Validate([]byte(data)); err != nil {
			t.Fatalf("valid response: %s %v", data, err)
		}
	}
}
