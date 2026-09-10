package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/AndrewDryga/coop/internal/config"
)

// MaxNativeConfigBytes bounds a native agent settings file coop reads into memory. These files sit
// in a directory the user AND the agent in the box both write, so their size is not coop's to
// trust: an oversized one is refused by name instead of being slurped whole into the process.
const MaxNativeConfigBytes = 4 << 20

func readDefaultsFile(path string) ([]byte, bool, error) {
	// Resolve the path first so the opened inode can be checked against it below. O_NOFOLLOW
	// refuses a symlink AT the path, but not an ordinary file swapped in after we looked.
	resolved, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil // removed between the lstat and the open: still just absent
		}
		if errors.Is(err, syscall.ELOOP) {
			// O_NOFOLLOW reports a symlink as ELOOP, which reads as a loop; name the real remedy.
			return nil, false, fmt.Errorf("%s is a symbolic link; coop reads agent settings without following links — replace it with a regular file and retry", path)
		}
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if err := verifyOpenedDefaults(path, resolved, opened); err != nil {
		return nil, false, err
	}
	// One byte past the cap, so a file that GREW between the stat and the read is refused here
	// rather than silently truncated into a value.
	data, err := io.ReadAll(io.LimitReader(f, MaxNativeConfigBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > MaxNativeConfigBytes {
		return nil, false, fmt.Errorf("read %s: agent settings grew past coop's %d-byte limit while it was being read", path, MaxNativeConfigBytes)
	}
	return data, true, nil
}

// verifyOpenedDefaults holds the file coop actually opened to the path it resolved: same inode,
// still a regular file, still within the byte cap. Split out because the swap it guards against is
// a race no test can drive through the reader itself.
func verifyOpenedDefaults(path string, resolved, opened os.FileInfo) error {
	if !opened.Mode().IsRegular() {
		return fmt.Errorf("read %s: expected a regular file, found %s", path, opened.Mode().Type())
	}
	if !os.SameFile(resolved, opened) {
		return fmt.Errorf("read %s: the file changed between resolving and opening it — retry", path)
	}
	if opened.Size() > MaxNativeConfigBytes {
		return fmt.Errorf("read %s: agent settings are %d bytes, over coop's %d-byte limit", path, opened.Size(), MaxNativeConfigBytes)
	}
	return nil
}

func readJSONDefaults(path string) (map[string]any, bool, error) {
	data, _, err := readDefaultsFile(path)
	if err != nil {
		return nil, false, err
	}
	values, blank, err := parseJSONDefaults(data)
	if err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return values, blank, nil
}

// parseJSONDefaults decodes one native settings document, returning bare errors the caller names
// the path in. Two things it does that json.Unmarshal would not: numbers decode as json.Number, so
// an account id or budget too large for a float64 survives the re-marshal that any change triggers;
// and anything after the top-level object is refused, which a Decoder otherwise reads as just the
// first document. A missing or blank file is a blank object, not an error.
func parseJSONDefaults(data []byte) (map[string]any, bool, error) {
	if len(data) > MaxNativeConfigBytes {
		return nil, false, fmt.Errorf("agent settings are over coop's %d-byte limit", MaxNativeConfigBytes)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, true, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		return nil, false, fmt.Errorf("invalid JSON: %w", err)
	}
	if values == nil {
		return nil, false, errors.New("expected a JSON object")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, false, errors.New("unexpected content after the JSON object")
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
