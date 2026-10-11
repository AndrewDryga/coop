package networkgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// Account discovery is a bounded document, unlike inference streams. Selection
// comes only from the guard's immutable account binding, never a default entry.
func projectNativeAccountResponse(response *http.Response, shape NativeBrokerAccountResponse, selected, alias string) error {
	refused := func() error { return Failure("native_broker_account_response_refused") }
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" {
		return refused()
	}
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	_ = response.Body.Close()
	if err != nil || len(data) > limit || !uniqueNativeJSON(data) {
		return refused()
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(data, &doc) != nil || doc == nil {
		return refused()
	}
	accounts := bytes.TrimSpace(doc[shape.CollectionKey])
	if len(accounts) == 0 {
		return refused()
	}
	encodedAlias, _ := json.Marshal(alias)
	found := 0
	orderingID := selected
	switch accounts[0] {
	case '[':
		var entries []map[string]json.RawMessage
		if json.Unmarshal(accounts, &entries) != nil {
			return refused()
		}
		for _, entry := range entries {
			var id string
			if json.Unmarshal(entry[shape.IDKey], &id) != nil {
				return refused()
			}
			if id == alias {
				return refused()
			}
			if id == selected {
				found++
				entry[shape.IDKey] = encodedAlias
			}
		}
		doc[shape.CollectionKey], err = json.Marshal(entries)
	case '{':
		var entries map[string]json.RawMessage
		var ordering []string
		if json.Unmarshal(accounts, &entries) != nil || json.Unmarshal(doc[shape.OrderingKey], &ordering) != nil {
			return refused()
		}
		if _, collision := entries[alias]; collision {
			return refused()
		}
		seen := map[string]bool{}
		var projected json.RawMessage
		for _, key := range ordering {
			if seen[key] {
				continue
			} // Native normalization removes an entry after use.
			seen[key] = true
			original, exists := entries[key]
			if !exists {
				continue // Native normalization skips stale ordering references.
			}
			var entry, account map[string]json.RawMessage
			if json.Unmarshal(original, &entry) != nil || json.Unmarshal(entry[shape.LegacyObjectKey], &account) != nil {
				return refused()
			}
			var id string
			if value := account[shape.LegacyIDKey]; len(value) != 0 && json.Unmarshal(value, &id) != nil {
				return refused()
			}
			if id == alias {
				return refused()
			}
			if id != selected {
				continue
			}
			found++
			account[shape.LegacyIDKey] = encodedAlias
			entry[shape.LegacyObjectKey], err = json.Marshal(account)
			if err != nil {
				return refused()
			}
			projected, err = json.Marshal(entry)
			if err != nil {
				return refused()
			}
			orderingID = key
		}
		if found != 1 {
			return refused()
		}
		delete(entries, orderingID)
		entries[alias] = projected
		// Do not synthesize routing from unknown legacy fields: the native client
		// deliberately discards those fields and must retain its missing-route denial.
		doc[shape.CollectionKey], err = json.Marshal(entries)
	default:
		return refused()
	}
	if err != nil || found != 1 {
		return refused()
	}
	if value := doc[shape.DefaultIDKey]; len(value) != 0 {
		var id string
		if json.Unmarshal(value, &id) != nil {
			return refused()
		}
		if id == selected {
			doc[shape.DefaultIDKey] = encodedAlias
		}
	}
	if value := doc[shape.OrderingKey]; len(value) != 0 {
		var ordering []string
		if json.Unmarshal(value, &ordering) != nil {
			return refused()
		}
		for i, id := range ordering {
			if id == orderingID {
				ordering[i] = alias
			}
		}
		doc[shape.OrderingKey], err = json.Marshal(ordering)
		if err != nil {
			return refused()
		}
	}
	data, err = json.Marshal(doc)
	if err != nil {
		return refused()
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	response.ContentLength = int64(len(data))
	response.TransferEncoding = nil
	response.Trailer = nil
	for _, name := range []string{"ETag", "Last-Modified", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Content-Encoding", "Transfer-Encoding", "Trailer"} {
		response.Header.Del(name)
	}
	response.Header.Set("Content-Length", strconv.Itoa(len(data)))
	response.Header.Set("Cache-Control", "no-store")
	return nil
}

// encoding/json accepts duplicate keys. Reject ambiguity before any projection,
// including inside unrelated metadata, while bounding hostile nesting depth.
func uniqueNativeJSON(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return true
		}
		switch delimiter {
		case '{':
			keys := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok || keys[key] || !value(depth+1) {
					return false
				}
				keys[key] = true
			}
		case '[':
			for decoder.More() {
				if !value(depth + 1) {
					return false
				}
			}
		default:
			return false
		}
		end, err := decoder.Token()
		return err == nil && (delimiter == '{' && end == json.Delim('}') || delimiter == '[' && end == json.Delim(']'))
	}
	if !value(0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}
