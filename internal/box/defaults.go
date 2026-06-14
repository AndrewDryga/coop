package box

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/config"
)

// ensureClaudeDefaults pre-answers Claude Code's first-run prompts so a fresh box
// goes straight to work instead of stopping for the theme picker, the folder-trust
// dialog, and the bypass-permissions warning. The box is itself the sandbox, so
// accepting bypass mode inside it is the intended posture. Existing values are
// preserved (account, credentials, the user's own settings); only missing flags
// are filled in, and the config file is rewritten only when something changes.
// workdir is the resolved cwd (see resolveWorkdir) so folder-trust is accepted
// for the path the agent actually runs in, across run/loop/acp.
func ensureClaudeDefaults(cfg *config.Config, workdir string) {
	dir := cfg.AgentDir("claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}

	// settings.json: a default theme (kept if the user already picked one) plus the
	// flag that skips Claude's bypass-permissions warning.
	settings := filepath.Join(dir, "settings.json")
	sm := map[string]any{}
	if data, err := os.ReadFile(settings); err == nil {
		_ = json.Unmarshal(data, &sm)
	}
	sChanged := false
	if _, ok := sm["theme"]; !ok {
		sm["theme"] = "dark"
		sChanged = true
	}
	if ensureTrue(sm, "skipDangerousModePermissionPrompt") {
		sChanged = true
	}
	if sChanged {
		writeJSONFile(settings, sm, 0o644)
	}

	// Accept flags in .claude.json, merged so account/onboarding state survives.
	cj := filepath.Join(dir, ".claude.json")
	m := map[string]any{}
	if data, err := os.ReadFile(cj); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	changed := ensureTrue(m, "hasCompletedOnboarding")
	if ensureTrue(m, "bypassPermissionsModeAccepted") {
		changed = true
	}
	if ensureWorkdirTrusted(m, workdir) {
		changed = true
	}
	if changed {
		writeJSONFile(cj, m, 0o600)
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

// ensureWorkdirTrusted marks the box workdir as a trusted project.
func ensureWorkdirTrusted(m map[string]any, workdir string) bool {
	projects, _ := m["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		m["projects"] = projects
	}
	wp, _ := projects[workdir].(map[string]any)
	if wp == nil {
		wp = map[string]any{}
		projects[workdir] = wp
	}
	if v, ok := wp["hasTrustDialogAccepted"].(bool); ok && v {
		return false
	}
	wp["hasTrustDialogAccepted"] = true
	return true
}

func writeJSONFile(path string, v any, perm os.FileMode) {
	if data, err := json.MarshalIndent(v, "", "  "); err == nil {
		_ = os.WriteFile(path, append(data, '\n'), perm)
	}
}
