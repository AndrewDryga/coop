package preset

import (
	"fmt"
	"os"
	"path/filepath"
)

// Template is the scaffolded preset — the documented frontier recipe, ready to edit:
// a big-model lead, a native deep-thinking subagent, a read-only cross-vendor critic,
// and a cheap write-capable delegate. Its prompt: lines are active because Scaffold
// also writes the files they reference (templateFiles); the result must load cleanly
// (TestScaffold loads it), so any referenced file MUST be in templateFiles.
const Template = `# coop preset — an orchestration recipe: which agent LEADS a session, and which
# ROLES it can route work to. Edit this file, then run or inspect it:
#   Run:      coop claude --preset %[1]s   ·   coop loop --preset %[1]s
#   Inspect:  coop presets %[1]s   ·   format: coop help presets
# A named agent overrides the lead; explicit --model / --credential override the
# ladder. Model ids: coop models. Accounts (logins): coop credentials. A preset
# names models and accounts, never the secrets themselves.

lead:                         # REQUIRED — the default agent + its model ladder.
  agent: claude               # REQUIRED — one of: claude, codex, gemini.

  # models — OPTIONAL model-first fallback ladder. Each entry is one of:
  #   <model>            bare model → runs on EVERY signed-in account, rotating
  #                      to the next on a rate limit.
  #   <model>@<account>  pins that model to one account (see coop credentials).
  #   {model: m, credential: a}   the same pair, written with keys.
  # A loop rotates it top-to-bottom (all accounts of entry 1, then entry 2, …),
  # keying rate limits per (model, account); a single run uses the first entry.
  # Omit models to use the agent's default model on all signed-in accounts.
  models: [claude-fable-5, claude-opus-4-8@work]

  # prompt — OPTIONAL Markdown appended to (never replacing) the generated lead
  # contract. init scaffolds roles/lead.md; edit it, or delete it and this line.
  prompt: roles/lead.md

# roles — OPTIONAL map of role-name → role the lead can hand work to. A role
# name is lowercase letters, digits, and dashes. Every role runs on its agent's
# DEFAULT account (only the lead rotates accounts). Each role sets a "mode"; the
# three below show one of each. Optional "when:" tags are routing hints the lead
# reads to decide when to use a role — they appear verbatim in its contract.
roles:

  # native — a Claude subagent that runs INSIDE the lead's own session (no extra
  # box). Requires agent: claude and a subagent: name.
  thinker:
    mode: native
    agent: claude
    model: claude-opus-4-8    # optional — omit for the agent's default model
    subagent: deep-reasoner   # required for native: a .claude/agents/ subagent
    when: [architecture, debugging, code-review, before-commit]

  # consult — a READ-ONLY peer invoked via coop-consult, for an independent
  # second opinion (often another vendor). Cannot edit files.
  critic:
    mode: consult
    agent: codex              # one of: claude, codex, gemini
    model: gpt-5.5
    when: [plan-review, security, tradeoffs]

  # delegate — a WRITE-CAPABLE worker invoked via coop-delegate. It may edit the
  # worktree but never commits (coop-delegate fails if HEAD moved) — the lead
  # reviews the diff, runs the gate, and commits.
  fast:
    mode: delegate
    agent: gemini
    model: gemini-3.5-flash
    when: [boilerplate, bulk-edits, test-scaffolding, repo-survey]
    commit: never             # delegate-only — only 'never' is supported
    concurrent: never         # delegate-only; only 'never' (runs serialized)
    prompt: roles/fast.md     # scaffolded; edit it, or delete it and this line
`

// leadPrompt and fastPrompt are the starter Markdown files the template references,
// written by Scaffold. They APPEND to coop's generated contract (they never replace
// its routing/safety text), so they carry project-specific guidance to fill in — a
// leading HTML note explains each and how to drop it.
const leadPrompt = `<!-- roles/lead.md — extra guidance for the LEAD, appended to (never
     replacing) coop's generated contract. Make it project-specific, then it is
     yours. Delete this file and the "prompt: roles/lead.md" line to drop it. -->

## This project (edit me)

- Gate: the one command that must pass before you commit (e.g. make check, npm test).
- Layout: where the code, tests, and docs live — anything a fresh session should know.
- Conventions: house style, naming, and what "done" means beyond a green gate.
`

const fastPrompt = `<!-- roles/fast.md — extra guidance for the "fast" delegate, appended to its
     generated contract. Delete this file and the "prompt: roles/fast.md" line in
     preset.yaml to drop it. -->

## Working as the fast delegate (edit me)

- Stay strictly within the task you are handed; note anything else, do not fix it.
- Follow the existing patterns and tests; add no new dependencies or options.
- You edit but never commit — leave the worktree gate-green for the lead to review.
`

// templateFile is one file Scaffold writes beside preset.yaml. Rel is a POSIX path
// relative to the preset folder (forward slashes; Scaffold localizes it).
type templateFile struct {
	rel     string
	content string
}

// templateFiles are the prompt files preset.yaml references — kept in step with the
// active prompt: lines in Template. A reference with no entry here would make the
// scaffolded preset fail to load, which TestScaffold catches.
var templateFiles = []templateFile{
	{"roles/lead.md", leadPrompt},
	{"roles/fast.md", fastPrompt},
}

// Scaffold writes the template as .agent/presets/<name>/preset.yaml plus the starter
// prompt files it references (templateFiles: roles/lead.md, roles/fast.md) and returns
// the preset.yaml path. It never clobbers an existing preset, and the result is guaranteed
// to load — the referenced prompt files are written here, so the active prompt: lines
// resolve.
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
	// The prompt files preset.yaml references, so the scaffolded preset loads as written.
	dir := filepath.Dir(path)
	for _, f := range templateFiles {
		dest := filepath.Join(dir, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dest, []byte(f.content), 0o644); err != nil {
			return "", err
		}
	}
	return path, nil
}
