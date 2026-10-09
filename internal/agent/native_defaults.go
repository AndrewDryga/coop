package agent

import (
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/AndrewDryga/coop/internal/safefile"
	"github.com/pelletier/go-toml/v2"
)

// A defaults seed is reviewed scalar preferences, not a profile copy. Project
// registries, auth, hooks, env and native MCP stores are deliberately excluded.
func nativeDefaultSettings(source, filename, instructions string, fields map[string]string) (map[string][]byte, error) {
	out := map[string][]byte{}
	root, err := safefile.OpenRoot(source)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if instructions != "" {
		data, err := safefile.ReadRegular(root, instructions, 1<<20)
		if err == nil {
			out[instructions] = data
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	data, err := safefile.ReadRegular(root, filename, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var old map[string]any
	isTOML := strings.HasSuffix(filename, ".toml")
	if isTOML {
		err = toml.Unmarshal(data, &old)
	} else {
		err = json.Unmarshal(data, &old)
	}
	if err != nil || old == nil {
		return nil, errors.New("host native defaults are malformed; source retained")
	}
	next := map[string]any{}
	for field, kind := range fields {
		parts := strings.Split(field, ".")
		object := old
		for _, part := range parts[:len(parts)-1] {
			child, ok := object[part].(map[string]any)
			if !ok {
				object = nil
				break
			}
			object = child
		}
		value, exists := object[parts[len(parts)-1]]
		if !exists {
			continue
		}
		valid := false
		switch kind {
		case "string":
			text, ok := value.(string)
			valid = ok && len(text) <= 4096 && !strings.ContainsAny(text, "\x00\r\n")
		case "bool":
			_, valid = value.(bool)
		}
		if !valid {
			return nil, errors.New("host native default has an unsupported value; source retained")
		}
		object = next
		for _, part := range parts[:len(parts)-1] {
			child, ok := object[part].(map[string]any)
			if !ok {
				child = map[string]any{}
				object[part] = child
			}
			object = child
		}
		object[parts[len(parts)-1]] = value
	}
	if len(next) > 0 {
		if isTOML {
			data, err = toml.Marshal(next)
		} else {
			data, err = json.Marshal(next)
		}
		if err != nil {
			return nil, err
		}
		out[filename] = data
	}
	return out, nil
}
