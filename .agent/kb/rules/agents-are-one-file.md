---
name: agents-are-one-file
description: "a coding agent is one self-registering file in `internal/agent`, never a switch elsewhere"
scope: architecture
sources: [internal/agent/agent.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/provider_decisions_test.go, Makefile]
check: "go test ./internal/agent -run 'TestRegistry|TestProviderDecisionsStayInAdapters|TestProviderDecisionGuard'"
updated: 2026-09-13
---

# A coding agent is one file in internal/agent — never a switch elsewhere

Every per-agent difference (commands, session resume, ACP binary, MCP translation,
first-run defaults, instruction filename, auth marker, npm packages) lives behind the
`Agent` interface in `internal/agent`. Each agent is one self-registering file
(`claude.go`, `codex.go`, `gemini.go`, `grok.go`); the rest of the codebase reaches agents through
the registry — `agents.Get(name)`, `agents.Valid(name)`, `agents.Names()`,
`agents.Default()`, `agents.Packages()`.

**Why:** Go's `switch` isn't exhaustive, so a hard-coded provider switch
in cli/box/consult means adding an agent is a scavenger hunt and the compiler won't catch
the case you miss (it just misbehaves silently — e.g. resume falls back to fresh). The
interface makes "answer every question for every agent" a compile-time requirement, and
adding an agent a single new file.

**How to apply:**
- Adding an agent → add `internal/agent/<name>.go` implementing `Agent` + `init(){ register(...) }`. Touch nothing else.
- Need a new per-agent behavior → add a method to the `Agent` interface (the compiler then forces every adapter to implement it) and have the caller use `agents.Get(name).Method()`.
- Never hard-code the provider set outside `internal/agent`. Validation is `agents.Valid`; the default agent is `agents.Default()`.
- The narrow exceptions are the `testdata` process-test oracles — `internal/cli/testdata/providerfixture` (native provider argv/output) and `internal/acpproxy/testdata/acpfixture` (per-provider ACP scripts): each is an independent oracle that must enumerate provider shapes instead of reusing production adapter code. Their registry-completeness tests must fail when a new adapter has no oracle arm.
- `make rules-check` runs the provider-decision guard. It parses Go sources, rejecting exact
  provider-name literals outside adapters while permitting comments and multiline examples.
  The recognized names come from the registry, not a second frozen provider list. Only Go
  tests and the two independent process-test oracle directories above are exempt.

## Changelog
- 2026-09-13 — swept production Go sources: moved the remaining review envelope, plain limit,
  model discovery, skills, scaffold and default selections behind the adapters. Strict host
  receipt grammar and CLI cache/execution policy remain with their owners. The guard now
  mechanically rejects selector/map/switch mutations, including a newly registered provider;
  only four prose/example literal lines remain in the textual census. Model protocol DTOs
  live below ACP control, preserving both opportunistic partial and forced strict decoding.
- 2026-09-10 — `Vendor()` joined the interface (the company behind the product: Anthropic, OpenAI,
  Google, xAI) for the launch line that says whose endpoints a filtered box may reach and who an
  offline agent cannot reach — the per-agent fact went behind the interface, not into a map in
  `box`. Guard grep unchanged: 0 new production literals outside `internal/agent`.
- 2026-09-06 — re-verified: the Codex commentary/final-answer phase filter that had grown inside
  `internal/sessionsvc/acp.go` moved behind `Agent.ACPFinalChunk(meta)` (codex answers from
  `_meta.codex.phase`, every other adapter says true); the guard grep fell from 27 hits to 26. The
  remaining hits are the fixture oracles and registry-side literals this card already allows.
- 2026-08-25 — removed the Fleet-board-only `Badge` presentation exception after deleting the
  board and the adapter method; all remaining per-agent production behavior stays in one file.
- 2026-08-10 — path-only: the fleet board (and with it `agentBadgeColors` +
  `TestAgentBadgeColors`) moved from `internal/cli/fleet_watch.go` to
  `internal/forkctl/fleet_watch.go`. The rule is unchanged: the color table still lives beside its
  ONLY renderer, one package away from the adapters.
- 2026-06-16 — created
- 2026-07-16 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — re-verified against `sources` while cutting the agent→ui import edge: badge COLOR
  moved to the presentation layer, recorded as an explicit exception (the adapters stay one file
  each and still answer `Badge()`)
