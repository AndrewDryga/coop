package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/AndrewDryga/coop/internal/config"
)

func readDefaultsFile(path string) ([]byte, bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("read %s: expected a regular file, found %s", path, info.Mode().Type())
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	return data, true, nil
}

func readJSONDefaults(path string) (map[string]any, bool, error) {
	data, exists, err := readDefaultsFile(path)
	if err != nil {
		return nil, false, err
	}
	blank := !exists || len(bytes.TrimSpace(data)) == 0
	if blank {
		return map[string]any{}, true, nil
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if values == nil {
		return nil, false, fmt.Errorf("parse %s: expected a JSON object", path)
	}
	return values, false, nil
}

func writeJSONFile(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := config.WriteFileAtomicMode(path, append(data, '\n'), perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ensureTrue sets m[key]=true unless it already is, reporting whether it changed.
func ensureTrue(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok && v {
		return false
	}
	m[key] = true
	return true
}
