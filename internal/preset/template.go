package preset

import (
	"fmt"
	"os"
	"path/filepath"
)

// Template is the scaffolded preset — the documented frontier recipe, ready to edit:
// a big-model lead, a native deep-thinking subagent, a read-only cross-vendor critic,
// and a cheap write-capable delegate. It must load cleanly as written (the test
// guards it), so the credentials/prompt lines ship commented — uncommenting
// credentials requires that account to be signed in, and a prompt: file must exist.
const Template = `# coop preset — an orchestration recipe: who leads, and which roles it routes work to.
# Run it:   coop claude --preset %[1]s   ·   coop loop --preset %[1]s
# Inspect:  coop presets %[1]s   ·   format reference: coop help presets

lead:
  agent: claude
  model: claude-fable-5
  # credentials: [work]     # the stored account(s) the lead runs on (see: coop credentials)
  # prompt: lead.md         # optional Markdown, appended to the generated contract

roles:
  thinker:                  # deep thinking + review, inside the lead's own session
    mode: native
    agent: claude
    model: claude-opus-4-8
    subagent: deep-reasoner # a .claude/agents/ subagent (coop init scaffolds this one)
    when: [architecture, debugging, code-review, before-commit]

  critic:                   # independent critique from another vendor — read-only
    mode: consult
    agent: codex
    model: gpt-5.5
    # credentials: [work]
    when: [plan-review, security, tradeoffs]

  fast:                     # cheap mechanical work — write-capable via coop-delegate
    mode: delegate
    agent: gemini
    model: gemini-3.5-flash
    # credentials: [work]
    when: [boilerplate, bulk-edits, test-scaffolding, repo-survey]
    commit: never           # it edits; the LEAD reviews the diff, runs the gate, commits
    concurrent: never       # delegate runs are serialized
    # prompt: roles/fast.md # optional Markdown, appended to this role's contract
`

// Scaffold writes the template as .agent/presets/<name>/preset.yaml and returns the
// written path. It never clobbers an existing preset, and the result is guaranteed to
// load (the template carries no uncommented file references).
func Scaffold(repo, name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("invalid preset name %q — a preset is a folder name under %s/ (lowercase, no '/', '..', or leading '-')", name, Dir)
	}
	path := Path(repo, name)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("preset %q already exists (%s) — edit it, or pick another name", name, filepath.Join(Dir, name, "preset.yaml"))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, fmt.Appendf(nil, Template, name), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
