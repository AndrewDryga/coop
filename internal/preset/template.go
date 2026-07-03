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
#   Run:      coop claude --preset %[1]s
#             coop loop --preset %[1]s
#   Inspect:  coop presets %[1]s
#   Format:   coop help presets
# A named agent overrides the lead; explicit --model / --credential override the
# ladder. Model ids: coop models. Accounts (logins): coop credentials. A preset
# names models and accounts, never the secrets themselves.

# lead — REQUIRED: the default agent for the session, plus its model ladder.
lead:
  # REQUIRED — one of: claude, codex, gemini.
  agent: claude

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

# roles — OPTIONAL map of role-name → role the lead can hand work to (a name is
# lowercase letters, digits, and dashes). Each role runs on its agent's DEFAULT
# account; only the lead rotates accounts. Every field is documented below.
roles:

  thinker:
    # mode: native — a Claude subagent that runs inside the lead's session (no
    # separate box; native requires agent: claude).
    mode: native
    # agent — one of: claude, codex, gemini.
    agent: claude
    # model — OPTIONAL model id (coop models); omit for the agent's default.
    model: claude-opus-4-8
    # subagent — REQUIRED for native: a .claude/agents/ subagent name.
    subagent: deep-reasoner
    # when — OPTIONAL routing hints the lead reads to pick this role.
    when: [architecture, debugging, code-review, before-commit]

  critic:
    # mode: consult — a READ-ONLY peer via coop-consult for a second opinion
    # (often another vendor); it cannot edit files.
    mode: consult
    # agent — one of: claude, codex, gemini.
    agent: codex
    # model — OPTIONAL; omit for the agent's default.
    model: gpt-5.5
    # when — OPTIONAL routing hints.
    when: [plan-review, security, tradeoffs]

  fast:
    # mode: delegate — a WRITE-CAPABLE worker via coop-delegate: it may edit the
    # worktree but never commits; the lead reviews the diff, gates, and commits.
    mode: delegate
    # agent — one of: claude, codex, gemini.
    agent: gemini
    # model — OPTIONAL; omit for the agent's default.
    model: gemini-3.5-flash
    # when — OPTIONAL routing hints.
    when: [boilerplate, bulk-edits, test-scaffolding, repo-survey]
    # commit — delegate-only; only "never" (the delegate never commits).
    commit: never
    # concurrent — delegate-only; only "never" (delegate runs are serialized).
    concurrent: never
    # prompt — OPTIONAL Markdown appended to this role's contract (see lead).
    prompt: roles/fast.md
`

// leadPrompt and fastPrompt are the starter Markdown files the template references,
// written by Scaffold. They APPEND to coop's generated contract (never replacing its
// routing/safety text) and hold sensible defaults that stand on their own for any
// project — a leading HTML note says to tune them, or how to drop them.
const leadPrompt = `<!-- roles/lead.md — guidance for the LEAD, appended to (never replacing) coop's
     generated contract. Sensible defaults for any project; tune for yours, or
     delete this file and the "prompt: roles/lead.md" line to drop it. -->

## How to work here

- Prefer the boring, proven approach; reach for something clever only when the
  simple one genuinely can't do the job — and say why.
- Understand before you change: read the surrounding code and match its style,
  naming, and structure instead of importing your own.
- Keep changes small and focused, one concern at a time; note unrelated problems
  rather than fixing them in the same pass.
- Done means verified: build it and run the tests (including the failure path);
  never claim something works before you have checked it.
- Handle the unhappy path — errors, empty input, edge cases — not just the demo.
- Leave the code better than you found it; never commit secrets or build junk.
`

const fastPrompt = `<!-- roles/fast.md — guidance for the "fast" delegate, appended to its generated
     contract. Sensible defaults for any project; tune for yours, or delete this
     file and the "prompt: roles/fast.md" line to drop it. -->

## Working as the fast delegate

- Stay strictly within the task you are handed; note anything else you notice,
  don't fix it in the same pass.
- Follow the existing patterns, style, and tests; add no new dependencies or
  options unless the task calls for them.
- Make the smallest change that does the job, and leave it formatted and clean.
- You may edit the worktree but must never commit — hand it back gate-green for
  the lead to review.
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
