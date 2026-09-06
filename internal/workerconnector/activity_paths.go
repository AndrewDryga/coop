package workerconnector

import (
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

var activityPathSource = regexp.MustCompile(`^/(input/(path|file_path|directory|paths/[0-9]{1,4})|(locations|content)/[0-9]{1,4}/path)$`)
var activityPathScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// Only bounded project-relative presentation facts cross this boundary. An
// outside/unknown path carries a warning, never the worker's absolute path.
func copyToolPathContext(public map[string]any, raw any) {
	context, ok := raw.(map[string]any)
	if !ok || context["basis"] != "lexical" {
		return
	}
	items, ok := context["paths"].([]any)
	if !ok {
		return
	}
	partial := context["partial"] == true || len(items) > 16
	paths := make([]map[string]any, 0, min(len(items), 16))
	for _, rawItem := range items[:min(len(items), 16)] {
		item, ok := rawItem.(map[string]any)
		if !ok {
			partial = true
			continue
		}
		source, _ := item["source"].(string)
		scope, _ := item["scope"].(string)
		if len(source) > 128 || !activityPathSource.MatchString(source) {
			partial = true
			continue
		}
		entry := map[string]any{"source": source, "scope": scope}
		switch scope {
		case "project":
			value, _ := item["path"].(string)
			if !publicProjectPath(value) {
				partial = true
				continue
			}
			entry["path"] = value
		case "outside", "unknown":
		default:
			partial = true
			continue
		}
		paths = append(paths, entry)
	}
	if len(paths) > 0 || partial {
		value := map[string]any{"basis": "lexical", "paths": paths}
		if partial {
			value["partial"] = true
		}
		public["path_context"] = value
	}
}

func publicProjectPath(value string) bool {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) ||
		path.IsAbs(value) || path.Clean(value) != value || value == ".." ||
		strings.HasPrefix(value, "../") || strings.HasPrefix(value, "~") ||
		strings.ContainsAny(value, "\x00\r\n\\") || activityPathScheme.MatchString(value) {
		return false
	}
	return true
}
