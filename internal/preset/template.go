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
# An explicit target on the command line (claude:opus-4.8@work) overrides the lead +
# ladder. Model ids: coop models. Accounts (logins): coop credentials. A preset names
# models and accounts, never the secrets themselves.

# lead — REQUIRED: the agent that leads the session, as a TARGET or a fallback ladder.
lead:
  # agent — REQUIRED. A target: provider[:model][@account], or a LIST of them (one provider):
  #   claude                          the agent's default model, on EVERY signed-in account
  #   claude:claude-opus-4-8          that model, on every signed-in account (rotating)
  #   claude:claude-opus-4-8@work     that model pinned to the "work" account only
  #   [claude:claude-fable-5, claude:claude-opus-4-8@work]   a fallback LADDER
  # A loop rotates the ladder top-to-bottom (all accounts of entry 1, then entry 2, …),
  # keying rate limits per (model, account); a single run uses the first entry. Model ids:
  # coop models. Accounts (logins): coop credentials.
  agent: [claude:claude-fable-5, claude:claude-opus-4-8@work]

  # prompt — OPTIONAL Markdown appended to (never replacing) the generated lead
  # contract. init scaffolds roles/lead.md; edit it, or delete it and this line.
  prompt: roles/lead.md

# roles — OPTIONAL map of role-name → role the lead can hand work to (a name is
# lowercase letters, digits, and dashes). Each role runs on its agent's DEFAULT
# account; only the lead rotates accounts. Every field is documented below.
roles:

  thinker:
    # mode: native — a Claude subagent that runs inside the lead's session (no
    # separate box; native is Claude-only). With no subagent: below, coop generates
    # a coop-thinker subagent in the box from this role — model, when, and prompt.
    mode: native
    # agent — a target: provider[:model]. The model (after the ':') is what the generated
    # subagent runs on (coop models); a bare provider uses the agent's default. A role runs
    # its agent's DEFAULT account — no @account (only the lead rotates accounts).
    agent: claude:claude-opus-4-8
    # when — OPTIONAL routing hints; become the subagent's description and the lead's cue.
    when: [architecture, debugging, code-review, before-commit]
    # prompt — the generated subagent's system prompt. To reference an existing
    # .claude/agents/ subagent instead of generating one, set: subagent: <name>.
    prompt: roles/thinker.md

  critic:
    # mode: consult — a READ-ONLY peer for a second opinion (often another
    # vendor), asked as coop-consult critic; it cannot edit files.
    mode: consult
    # agent — a target: provider[:model] (one of claude, codex, gemini). The model is
    # optional; omit it (agent: codex) for the agent's default.
    agent: codex:gpt-5.5
    # when — OPTIONAL routing hints.
    when: [plan-review, security, tradeoffs]
    # prompt — OPTIONAL persona the peer adopts for this role's consults
    # (e.g. prompt: roles/critic.md); omit and the peer answers as itself.

  fast:
    # mode: delegate — a WRITE-CAPABLE worker via coop-delegate: it may edit the
    # worktree but never commits; the lead reviews the diff, gates, and commits.
    mode: delegate
    # agent — a target: provider[:model] (one of claude, codex, gemini); omit the model
    # (agent: gemini) for the agent's default.
    agent: gemini:gemini-3.5-flash
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

## Route before you write

Before writing code, classify the change: JUDGMENT (design, tricky logic,
anything worth reasoning about) or MECHANICAL (you could specify it exactly in
a few sentences). Mechanical work goes to your delegate role by default — keep
your context for leading, not typing. If you catch yourself grinding out
repetitive edits by hand, stop and hand them off.
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

// thinkerPrompt is the generated coop-thinker subagent's system prompt (the native
// "thinker" role has no subagent:, so coop generates one from this). Unlike lead/fast it
// isn't appended to a contract — it IS the subagent's instructions, so it reads as one.
const thinkerPrompt = `<!-- roles/thinker.md — the generated coop-thinker subagent's system prompt (the
     thinker role in preset.yaml). Tune it, or reference an existing subagent with
     "subagent: <name>" and delete this file. -->

You are the deep-reasoning specialist the lead delegates hard thinking to.

Think the problem through before concluding: the alternatives, their failure modes, and
what evidence in the repo supports or contradicts each. Read whatever code you need —
verify claims against the actual source rather than assuming.

Your reply is consumed by the lead, not a human: lead with the decision or diagnosis,
then the load-bearing reasoning in a few sentences, then concrete next steps. No preamble,
and no survey of rejected options unless a rejection is the insight.
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
	{"roles/thinker.md", thinkerPrompt},
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
	path := Path(repo, "", name) // scaffolding is repo-only; global authoring is by hand
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
