package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// OutputValidator is the compiled form of one admitted output contract.
// It is intentionally opaque so callers cannot validate against a different
// schema than the durable turn carries.
type OutputValidator struct {
	schema *jsonschema.Schema
}

func CompileOutputContract(contract *OutputContract) (*OutputValidator, error) {
	if contract == nil {
		return nil, nil
	}
	if len(contract.JSONSchema) == 0 || len(contract.JSONSchema) > MaxOutputSchemaBytes {
		return nil, &Error{Code: CodeInvalidRequest, Detail: "output contract schema exceeds its bound"}
	}
	digest := sha256.Sum256(contract.JSONSchema)
	if contract.SHA256 != hex.EncodeToString(digest[:]) {
		return nil, &Error{Code: CodeInvalidRequest, Detail: "output contract digest does not match its schema"}
	}
	document, err := decodeUniqueJSON(contract.JSONSchema)
	if err != nil {
		return nil, &Error{Code: CodeInvalidRequest, Detail: "output contract is not valid JSON Schema"}
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	name := "urn:coop:output-contract:" + contract.SHA256
	if err := compiler.AddResource(name, document); err != nil {
		return nil, &Error{Code: CodeInvalidRequest, Detail: "output contract is not valid JSON Schema"}
	}
	compiled, err := compiler.Compile(name)
	if err != nil {
		return nil, &Error{Code: CodeInvalidRequest, Detail: "output contract is not valid JSON Schema"}
	}
	return &OutputValidator{schema: compiled}, nil
}

func (v *OutputValidator) Validate(data []byte) error {
	if v == nil || v.schema == nil {
		return nil
	}
	value, err := decodeUniqueJSON(data)
	if err != nil {
		return fmt.Errorf("final response is not one unique-property JSON value: %w", err)
	}
	if err := v.schema.Validate(value); err != nil {
		return fmt.Errorf("final response does not match the output contract: %w", err)
	}
	return nil
}

func cloneOutputContract(value *OutputContract) *OutputContract {
	if value == nil {
		return nil
	}
	return &OutputContract{
		JSONSchema: append(json.RawMessage(nil), value.JSONSchema...),
		SHA256:     value.SHA256,
	}
}

// decodeUniqueJSON rejects trailing values and duplicate object properties.
// encoding/json otherwise silently keeps the last duplicate, which would let
// a provider and a caller interpret the same supposedly validated bytes differently.
func decodeUniqueJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeUniqueJSONToken(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func decodeUniqueJSONToken(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		result := make(map[string]any)
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, ok := nameToken.(string)
			if !ok {
				return nil, errors.New("object property is not a string")
			}
			if _, exists := result[name]; exists {
				return nil, fmt.Errorf("duplicate object property %q", name)
			}
			value, err := decodeUniqueJSONToken(decoder)
			if err != nil {
				return nil, err
			}
			result[name] = value
		}
		if closeToken, err := decoder.Token(); err != nil || closeToken != json.Delim('}') {
			return nil, errors.New("object is not closed")
		}
		return result, nil
	case '[':
		var result []any
		for decoder.More() {
			value, err := decodeUniqueJSONToken(decoder)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		if closeToken, err := decoder.Token(); err != nil || closeToken != json.Delim(']') {
			return nil, errors.New("array is not closed")
		}
		return result, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}
