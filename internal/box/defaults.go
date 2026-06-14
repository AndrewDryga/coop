package box

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// ensureCodexDefaults pre-trusts the workdir in Codex's config.toml so a fresh box
// doesn't stop at Codex's "Do you trust this directory?" prompt. Codex records
// trust as [projects."<dir>"] trust_level = "trusted" in ~/.codex/config.toml; we
// append that entry (idempotently) to the mounted config. The box is itself the
// sandbox, so trusting the one mounted repo is the intended posture. It runs
// before MCP generation so the merged config carries the entry on the first run.
func ensureCodexDefaults(cfg *config.Config, workdir string) {
	if workdir == "" {
		return
	}
	dir := cfg.AgentDir("codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "config.toml")
	data, _ := os.ReadFile(path) // missing file → empty, which is fine
	// Leave any existing entry for this dir (the user's, or Codex's own) untouched.
	if strings.Contains(string(data), fmt.Sprintf("projects.%q", workdir)) {
		return
	}
	out := string(data)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if out != "" {
		out += "\n"
	}
	out += fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"\n", workdir)
	os.WriteFile(path, []byte(out), 0o644)
}

// ensureGeminiDefaults makes sure Gemini's settings.json is valid JSON so the CLI
// starts: an empty or missing settings.json makes gemini fail at launch with
// "Unexpected end of JSON input". We seed "{}" only when the file is missing or
// blank, so any settings you've added are left untouched. It runs before MCP
// generation, which reads this file to merge in servers.
func ensureGeminiDefaults(cfg *config.Config) {
	dir := cfg.AgentDir("gemini")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "settings.json")
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
		return // has content (yours to keep, or to fix) — don't clobber it
	}
	os.WriteFile(path, []byte("{}\n"), 0o644)
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
