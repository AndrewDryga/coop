package preset

// Template is the scaffolded preset — the documented frontier recipe, ready to edit:
// a big-model lead with a cross-provider fallback, a read-only deep-thinking consult, a
// read-only cross-vendor critic, and a cheap write-capable delegate. Its comments say what a person has to decide,
// not everything the loader accepts — `coop help presets` is the reference. The
// prompt: lines are active because Scaffold also writes the files they reference
// (templateFiles); the result must load cleanly (TestScaffold loads it), so any
// referenced file MUST be in templateFiles.
const Template = `# Models and providers that work together.
# Run: coop %[1]s
# Inspect: coop presets %[1]s
# Learn how presets work: coop help presets

lead:
  # The lead owns the session and combines the roles' work.
  # A list tries agents in order where automatic rotation is supported.
  # Model, effort and account syntax: coop help models
  agent: [claude:claude-fable-5/xhigh, codex:gpt-5.6-sol/xhigh]

  # Extra instructions for the lead. Omit this field when not needed.
  prompt: roles/lead.md

roles:
  thinker:
    # Consult roles give read-only advice.
    # mode: native would run this role inside the lead's own session instead,
    # which needs every lead above to be the role's provider.
    mode: consult
    agent: claude:claude-opus-4-8/xhigh
    when: [architecture, debugging, code-review, before-commit]
    prompt: roles/thinker.md

  critic:
    # Consult roles give read-only advice.
    mode: consult
    # Roles use the provider's default account; do not add @account.
    # Consult and delegate agents may also be a fallback list.
    agent: codex:gpt-5.6-sol/xhigh
    when: [plan-review, security, tradeoffs]
    prompt: roles/critic.md

  fast:
    # Delegates edit files. The lead reviews, checks and commits their work.
    mode: delegate
    agent: gemini:gemini-3.5-flash
    when: [boilerplate, bulk-edits, test-scaffolding, repo-survey]
    # These delegate settings only accept never.
    commit: never
    concurrent: never
    prompt: roles/fast.md
`

// leadPrompt and fastPrompt are the starter Markdown files the template references, written by
// Scaffold. They hold sensible defaults that stand on their own for any project, and each opens
// with the same one line: what the file is, and how to drop it. How a prompt reaches its agent
// differs by role — appended to a generated contract, or the whole native subagent file — so no
// starter claims one of the two.
const leadPrompt = `<!-- Extra instructions for this agent. Remove the prompt setting to omit this file. -->

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

const fastPrompt = `<!-- Extra instructions for this agent. Remove the prompt setting to omit this file. -->

## Working as the fast delegate

- Stay strictly within the task you are handed; note anything else you notice,
  don't fix it in the same pass.
- Follow the existing patterns, style, and tests; add no new dependencies or
  options unless the task calls for them.
- Make the smallest change that does the job, and leave it formatted and clean.
- You may edit the worktree but must never commit — hand it back gate-green for
  the lead to review.
`

// criticPrompt is the persona the "critic" consult adopts — the second opinion from another
// vendor. It carries only the stance, never the role's wiring.
const criticPrompt = `<!-- Extra instructions for this agent. Remove the prompt setting to omit this file. -->

## Working as the critic

You are the second opinion, asked precisely because you did not write the plan.

- Give the verdict first — does it hold? — then the strongest objection you have and
  what would have to be true for it to matter.
- Weigh the option that was not chosen; if it is better, say so in one sentence and why.
- Name every one-way door: a schema, a file format, a published flag, stored data —
  anything a later change cannot take back.
- You read; you never edit. Answer in a few dense sentences, no preamble and no praise.
`

// thinkerPrompt is the persona the "thinker" consult adopts. It reads as instructions to the role
// itself, so it serves unchanged if the role is made native (a generated subagent's system prompt).
const thinkerPrompt = `<!-- Extra instructions for this agent. Remove the prompt setting to omit this file. -->

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
	{"roles/critic.md", criticPrompt},
	{"roles/fast.md", fastPrompt},
}
