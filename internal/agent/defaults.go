package agent

import (
	"encoding/json"
	"os"
)

// writeJSONFile writes v as indented JSON. Shared by the adapters' EnsureDefaults.
func writeJSONFile(path string, v any, perm os.FileMode) {
	if data, err := json.MarshalIndent(v, "", "  "); err == nil {
		_ = os.WriteFile(path, append(data, '\n'), perm)
	}
}

// ensureTrue sets m[key]=true unless it already is, reporting whether it changed.
func ensureTrue(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok && v {
		return false
	}
	m[key] = true
	return true
}
