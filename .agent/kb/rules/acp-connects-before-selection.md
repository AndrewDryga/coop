---
name: acp-connects-before-selection
description: coop acp and coop fork <name> acp connect without a target so the editor can expose its live selectors
scope: cli-grammar
sources: [internal/cli/acp_cmd.go, internal/cli/fork_cmd.go, internal/cli/acp_startup_test.go, internal/cli/fork_cmd_test.go, internal/acpproxy/scripted_startup_e2e_test.go, internal/cli/help.go, README.md, MIGRATING.md]
check: go test ./internal/cli -run 'TestACPAutomaticStartup|TestForkACPWithoutTargetStartsTheForksAgent'
updated: 2026-10-04
---

# Connect ACP before asking the editor to select a provider

An editor configured with `["acp"]` must connect when a provider is signed in. Choose the first
signed-in provider in registry order and its marked default account, without turning that
automatic choice into an explicit pin. Preserve explicit target, preset and supervisor selection
precedence. Keep `--bare` separate: it still requires an explicit single target.

A fork's entry, `["fork", "<name>", "acp"]`, connects the same way. It starts the agent the fork
was created with, or the first signed-in provider when the fork has none saved. Its provider and
account stay fixed, and the editor's native selectors choose the model and effort.

**Why:** the user asked to restore the previous selector workflow. Requiring a target before
connection prevents the editor from showing the very selectors intended to choose that target.
For forks, the user (2026-10-04): "you should be able to coop fork myfork acp and then select
model in the editor".

**How to apply:** test the literal no-target argv through the external process harness, including
a provider switch and SIGHUP. An empty positional token is not the same invocation. Missing
sign-in must explain the login action on stderr without corrupting ACP stdout. Network policy
still governs which providers can run; automatic startup must never silently widen it.

## Changelog
- 2026-10-04 — extended to `coop fork <name> acp`, which refused to start without a target.
  Swept the ACP entry points: plain `coop acp` already complied; the fork entry now starts its
  saved agent (TestForkACPWithoutTargetStartsTheForksAgent, and TestScriptedForkACPStartsWithoutTarget
  through the real supervisor); session daemon children always receive the daemon's target, and
  `--bare` keeps its required target. Both share one first-signed-in helper.
- 2026-09-12 — restored startup and swept the six listed source/test/doc surfaces. Removed the
  obsolete required-target regression and migration instructions; explicit targets and `--bare`
  keep their existing contracts. CLI regressions and literal startup/switch/reload scripts pass.
