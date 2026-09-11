---
name: acp-connects-before-selection
description: plain coop acp connects automatically so the editor can expose its live selectors
scope: cli-grammar
sources: [internal/cli/acp_cmd.go, internal/cli/acp_startup_test.go, internal/acpproxy/scripted_startup_e2e_test.go, internal/cli/help.go, README.md, MIGRATING.md]
check: go test ./internal/cli -run TestACPAutomaticStartup
updated: 2026-09-12
---

# Connect ACP before asking the editor to select a provider

An editor configured with `["acp"]` must connect when a provider is signed in. Choose the first
signed-in provider in registry order and its marked default account, without turning that
automatic choice into an explicit pin. Preserve explicit target, preset and supervisor selection
precedence. Keep `--bare` separate: it still requires an explicit single target.

**Why:** the user asked to restore the previous selector workflow. Requiring a target before
connection prevents the editor from showing the very selectors intended to choose that target.

**How to apply:** test the literal no-target argv through the external process harness, including
a provider switch and SIGHUP. An empty positional token is not the same invocation. Missing
sign-in must explain the login action on stderr without corrupting ACP stdout. Network policy
still governs which providers can run; automatic startup must never silently widen it.

## Changelog
- 2026-09-12 — restored startup and swept the six listed source/test/doc surfaces. Removed the
  obsolete required-target regression and migration instructions; explicit targets and `--bare`
  keep their existing contracts. CLI regressions and literal startup/switch/reload scripts pass.
