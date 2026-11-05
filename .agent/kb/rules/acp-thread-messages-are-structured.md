---
name: acp-thread-messages-are-structured
description: A thread message has its title alone on a line, short paragraphs, and each command to run as a fenced code block
scope: cli-output
sources: [internal/acpctl/start_notice.go, internal/acpctl/control.go, internal/acpproxy/proxy.go, internal/acpproxy/thread_tail.go, internal/cli/acp_box_wait.go, internal/cli/acp_refresh.go, internal/cli/acp_start_notice.go, internal/testutil/threadtext/threadtext.go]
check: make thread-messages-check
updated: 2026-11-05
---

# Thread messages read top down, and their commands can be copied

An editor renders what Coop writes into a thread as Markdown. Write for that:

- A title goes alone on its line, then a blank line: `⚠ **Title.**` for something to fix, `**Title.**` for a
  state. Never run a sentence on after it.
- Separate the sections of a message with a `---` rule: the ports, the services, the network, each problem. Zed
  puts space under a paragraph and none under a list, so without one a title after a list sits closer to the list
  than to its own text. Never rule between the parts of one section: its follow-up lines are plain paragraphs.
- No reassurance lines a capable reader does not need ("Open threads pick it up on their own").
- Short paragraphs follow, one thought each. Several items are a list; one item stays a sentence.
- Every command the person should run is its own fenced code block (`acpctl.CodeBlock`), introduced by a line
  saying where to run it ("Run this in this project, on the host:"). An editor offers to copy a block whole. Never
  `Run coop approve` or `` Run `coop approve` `` in a sentence.
- Only what the person copies goes in a code block: a command, or rules to paste. Zed does not wrap one, so a
  sentence inside runs off the panel's edge. Hard-wrapping it does not help, since panel widths vary.
- Build problems with `acpctl.ProblemText`, so they all look the same.
- Set a message apart with `acpproxy.ThreadText`, which the message builders already do. An editor appends the
  chunks of a running message with nothing between them, and a closing fence with the agent's next words on its
  line does not close, so the rest of the message becomes code.
- Build a message with `acpproxy.SessionMessageLine`. The writer to the editor (`threadTail`) knows what the thread
  last showed, and opens a message that follows agent text with a `---` rule. Do not write that rule yourself: an
  empty thread must not start with one.

Wrong: `**Internet is filtered.** Allowed: the agent's provider.`
Right: `**Internet is filtered.**`, a blank line, `Allowed: the agent's provider.`

Errors too: Zed renders a thread error and an auth-required description as Markdown, so a sign-in error carries a
code block. Only `LoadError::Other` (a `session/new` failure that is not auth) is plain text, where the fence shows
literally and still reads.

## Changelog
- 2026-11-05 — the owner saw rules between the network section's own parts ("To see them all", "To allow
  another site") and a closing "Open threads pick it up on their own": rules now only set sections apart, and the
  sentence is gone. Sweep: the network text was the only one ruling inside a section.
- 2026-11-04 — the owner saw a notice right under the last reply with nothing between: a message after agent text
  now opens with a rule. Sweep: Coop's two line builders are one now, so every notice goes through it.
- 2026-11-04 — the owner saw a title closer to the list above it than to its own text: sections now take a `---` rule.
- 2026-11-03 — created after the owner asked for this. Sweep: every text in `internal/acpctl`, the session texts in
  `internal/cli/acp_box_wait.go`, `acp_refresh.go` and `acp_start_notice.go`, and `internal/acpproxy/proxy.go`.
  Found and fixed: the network block, problems, the box wait, refresh notices, sign-in errors, the replay failure,
  the session-ended notice, the setting-failed notice and the loopback warning. Plain one-line statuses (a port is
  up, an account hit its limit) stay one line. Graduated into `threadtext.Check` from the start.
- 2026-11-04 — the owner saw a services refusal cut off in Zed, since all of it sat in one code block. Only what
  to copy goes in a block now, and `threadtext.Check` fails a block line that ends like a sentence. Sweep: that
  refusal was the only text with a sentence in a block; the rest of the thread texts pass the new check.
- 2026-11-04 — why a project file is broken sat in a block with no stop at its end, so the check missed it and up to
  600 characters ran off the panel. It is one escaped line of text now, and the check also fails a long line of many
  plain words. Sweep: no other thread text trips the wider check.
