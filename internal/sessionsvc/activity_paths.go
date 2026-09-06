package sessionsvc

import (
	"bytes"
	"encoding/json"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var activityPathScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// This describes the provider's reported path relative to the bound checkout.
// It deliberately does no filesystem I/O and grants no containment authority:
// a project-relative symlink may still point outside the project.
type activityPaths struct {
	Basis   string         `json:"basis"`
	Paths   []activityPath `json:"paths"`
	Partial bool           `json:"partial,omitempty"`
}

type activityPath struct {
	Source string `json:"source"`
	Path   string `json:"path,omitempty"`
	Scope  string `json:"scope"`
}

func activityPathContext(root, kind string, input, locations, content json.RawMessage) activityPaths {
	result := activityPaths{Basis: "lexical", Paths: []activityPath{}}
	add := func(source string, raw any) {
		value, ok := raw.(string)
		if !ok {
			return
		}
		if len(result.Paths) >= 16 {
			result.Partial = true
			return
		}
		display, scope := activityRelativePath(root, value)
		result.Paths = append(result.Paths, activityPath{Source: source, Path: display, Scope: scope})
	}
	if kind == "read" || kind == "edit" || kind == "search" || kind == "delete" || kind == "move" {
		var fields map[string]any
		if len(input) > sessionActivityInputBytes {
			result.Partial = true
		}
		if len(input) <= sessionActivityInputBytes && json.Unmarshal(input, &fields) == nil && fields["server"] == nil && fields["truncated"] != true {
			for _, key := range []string{"path", "file_path", "directory"} {
				add("/input/"+key, fields[key])
			}
			if paths, ok := fields["paths"].([]any); ok {
				for i, p := range paths {
					add("/input/paths/"+strconv.Itoa(i), p)
					if len(result.Paths) >= 16 && i+1 < len(paths) {
						result.Partial = true
						break
					}
				}
			}
		}
	}
	for _, field := range []struct {
		source string
		raw    json.RawMessage
		diff   bool
	}{
		{"/locations/", locations, false}, {"/content/", content, true},
	} {
		if len(field.raw) == 0 || string(field.raw) == "null" {
			continue
		}
		if len(field.raw) > sessionACPFrameLimit {
			result.Partial = true
			continue
		}
		// Diff bodies can fill a whole bounded ACP frame. Decode only the typed
		// path fields before evidence previews discard oldText/newText, and cap
		// the number of inspected entries independently of the frame byte bound.
		decoder := json.NewDecoder(bytes.NewReader(field.raw))
		if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
			result.Partial = true
			continue
		}
		for i := 0; i < 16 && decoder.More(); i++ {
			var item struct {
				Type string  `json:"type"`
				Path *string `json:"path"`
			}
			if decoder.Decode(&item) != nil {
				result.Partial = true
				break
			}
			if item.Path != nil && (!field.diff || item.Type == "diff") {
				add(field.source+strconv.Itoa(i)+"/path", *item.Path)
			}
		}
		if decoder.More() {
			result.Partial = true
		}
	}
	return result
}

func activityRelativePath(root, value string) (string, string) {
	if root == "" || !path.IsAbs(root) || path.Clean(root) == "/" || value == "" ||
		!utf8.ValidString(value) || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n\\") ||
		strings.HasPrefix(value, "~") || activityPathScheme.MatchString(value) {
		return "", "unknown"
	}
	root = path.Clean(root)
	absolute := value
	if !path.IsAbs(value) {
		absolute = path.Join(root, value)
	}
	absolute = path.Clean(absolute)
	if absolute == root {
		return ".", "project"
	}
	if !strings.HasPrefix(absolute, root+"/") {
		return "", "outside"
	}
	relative := strings.TrimPrefix(absolute, root+"/")
	if len(relative) > 512 || activityPathScheme.MatchString(relative) {
		return "", "unknown"
	}
	return relative, "project"
}

func mergeActivityPaths(groups ...activityPaths) activityPaths {
	result := activityPaths{Basis: "lexical", Paths: []activityPath{}}
	for _, group := range groups {
		result.Partial = result.Partial || group.Partial
		for _, entry := range group.Paths {
			if len(result.Paths) == 16 {
				result.Partial = true
				break
			}
			result.Paths = append(result.Paths, entry)
		}
	}
	return result
}
