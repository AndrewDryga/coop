# A coding agent is one file in internal/agent — never a switch elsewhere

Every per-agent difference (commands, session resume, ACP binary, MCP translation,
first-run defaults, instruction filename, auth marker, npm packages) lives behind the
`Agent` interface in `internal/agent`. Each agent is one self-registering file
(`claude.go`, `codex.go`, `gemini.go`, `grok.go`); the rest of the codebase reaches agents through
the registry — `agents.Get(name)`, `agents.Valid(name)`, `agents.Names()`,
`agents.Default()`, `agents.Packages()`.

**Why:** Go's `switch` isn't exhaustive, so a hard-coded provider switch
in cli/box/fusion means adding an agent is a scavenger hunt and the compiler won't catch
the case you miss (it just misbehaves silently — e.g. resume falls back to fresh). The
interface makes "answer every question for every agent" a compile-time requirement, and
adding an agent a single new file.

**How to apply:**
- Adding an agent → add `internal/agent/<name>.go` implementing `Agent` + `init(){ register(...) }`. Touch nothing else.
- Need a new per-agent behavior → add a method to the `Agent` interface (the compiler then forces every adapter to implement it) and have the caller use `agents.Get(name).Method()`.
- Never hard-code the provider set outside `internal/agent`. Validation is `agents.Valid`; the default agent is `agents.Default()`.
- The narrow exception is `internal/cli/testdata/providerfixture`: it is an independent process-test oracle, so it must enumerate native provider argv/output shapes instead of reusing production adapter code. Its registry-completeness test must fail when a new adapter has no oracle arm.
- Guard production code only: `rg '\"(claude|codex|gemini|grok)\"' internal -g '!**/*_test.go' -g '!internal/agent/**' -g '!internal/cli/testdata/providerfixture/**'` should return nothing.
