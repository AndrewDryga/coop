package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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

// A controller's schema stands alone. A reference to a local file was once opened by the worker,
// and values from that file came back in validation errors.
func TestOutputContractRefusesExternalReferences(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(secret, []byte(`{"const":"SECRET-VALUE"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	contract := func(schema string) *OutputContract {
		digest := sha256.Sum256([]byte(schema))
		return &OutputContract{SHA256: hex.EncodeToString(digest[:]), JSONSchema: json.RawMessage(schema)}
	}
	for _, schema := range []string{
		`{"$ref":"file://` + secret + `"}`,
		`{"$schema":"file://` + secret + `"}`,
		`{"properties":{"a":{"$ref":"file://` + secret + `#/const"}}}`,
		`{"$ref":"https://example.com/schema.json"}`,
		`{"$ref":"file:///nonexistent/coop-audit.json"}`,
	} {
		validator, err := CompileOutputContract(contract(schema))
		if CodeOf(err) != CodeInvalidRequest {
			t.Errorf("schema %s compiled (err %v)", schema, err)
			if validator != nil {
				t.Logf("validation of \"other\": %v", validator.Validate([]byte(`"other"`)))
			}
		}
		if err != nil && strings.Contains(err.Error(), "SECRET-VALUE") {
			t.Errorf("the refusal quotes the file: %v", err)
		}
	}
	for _, schema := range []string{
		`{"$defs":{"name":{"type":"string"}},"type":"object","properties":{"name":{"$ref":"#/$defs/name"}}}`,
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string"}`,
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"string"}`,
	} {
		if _, err := CompileOutputContract(contract(schema)); err != nil {
			t.Errorf("self-contained schema %s was refused: %v", schema, err)
		}
	}
}
