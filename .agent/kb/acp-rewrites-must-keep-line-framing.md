---
name: acp-rewrites-must-keep-line-framing
description: an ACP line rewrite that drops the trailing newline hangs the session silently — no error, no timeout, and unit tests that parse the line still pass
subsystem: acp
sources: [internal/acpctl/control.go, internal/acpproxy/proxy.go]
updated: 2026-08-24
---
ACP is newline-delimited JSON. Every builder in `internal/acpctl/control.go` therefore ends with
`append(out, '\n')` — `sessionMessage`, `wrapPromptLine`, `canonicalCwd`, the lot. `json.Marshal`
does NOT add one.

Drop the terminator and there is no error to find: the adapter has a complete JSON object sitting in
its buffer and goes on waiting for the end of the line. `session/prompt` or `session/new` simply
never returns. Nothing logs, nothing times out, and the wire trace looks CORRECT — the rewritten
line appears under `editor→box(coop-rewrite)` with exactly the content you intended.

Observed 2026-08-24 adding `canonicalCwd`: the trace showed the rewritten cwd going out and the old
`-32602 cwd does not exist` error gone, and the session still hung forever.

**Unit tests do not catch this.** A test that unmarshals the rewritten line and asserts on its
fields passes identically with and without the terminator — `encoding/json` neither needs nor
minds it. Assert the FRAMING separately:

```go
if !bytes.HasSuffix(line, []byte("\n")) {
    t.Errorf("rewritten line is not newline-terminated:\n%q", line)
}
```

**Isolating it.** A hang like this looks like a broken box. The cheap discriminator is an input that
takes the SAME path but skips the rewrite (here: a cwd already spelled canonically). If that returns
in seconds, the box is healthy and the rewrite is the whole story. See [[acp-scripted-e2e]] for
driving the real supervisor/control/proxy path when a unit test cannot reach the bug.

## Changelog
- 2026-08-24 — created after a cwd-normalizing rewrite in `fromEditor` shipped without its trailing
  newline and hung `session/new` indefinitely while its unit tests were green; verified against
  `wrapPromptLine`/`sessionMessage`, which have always appended one.
