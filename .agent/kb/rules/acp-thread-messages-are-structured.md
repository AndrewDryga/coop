---
name: acp-thread-messages-are-structured
description: A thread message has its title alone on a line, short paragraphs, and each command to run as a fenced code block
scope: cli-output
sources: [internal/acpctl/start_notice.go, internal/acpctl/control.go, internal/acpproxy/proxy.go, internal/cli/acp_box_wait.go, internal/cli/acp_refresh.go, internal/cli/acp_start_notice.go, internal/testutil/threadtext/threadtext.go]
check: make thread-messages-check
updated: 2026-11-04
---

# Thread messages read top down, and their commands can be copied

An editor renders what Coop writes into a thread as Markdown. Write for that:

- A title goes alone on its line, then a blank line: `⚠ **Title.**` for something to fix, `**Title.**` for a
  state. Never run a sentence on after it.
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

Wrong: `**Internet is filtered.** Allowed: the agent's provider.`
Right: `**Internet is filtered.**`, a blank line, `Allowed: the agent's provider.`

Errors too: Zed renders a thread error and an auth-required description as Markdown, so a sign-in error carries a
code block. Only `LoadError::Other` (a `session/new` failure that is not auth) is plain text, where the fence shows
literally and still reads.

## Changelog
- 2026-11-03 — created after the owner asked for this. Sweep: every text in `internal/acpctl`, the session texts in
  `internal/cli/acp_box_wait.go`, `acp_refresh.go` and `acp_start_notice.go`, and `internal/acpproxy/proxy.go`.
  Found and fixed: the network block, problems, the box wait, refresh notices, sign-in errors, the replay failure,
  the session-ended notice, the setting-failed notice and the loopback warning. Plain one-line statuses (a port is
  up, an account hit its limit) stay one line. Graduated into `threadtext.Check` from the start.
- 2026-11-04 — the owner saw a services refusal cut off in Zed, since all of it sat in one code block. Only what
  to copy goes in a block now, and `threadtext.Check` fails a block line that ends like a sentence. Sweep: that
  refusal was the only text with a sentence in a block; the rest of the thread texts pass the new check.
