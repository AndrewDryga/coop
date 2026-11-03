---
name: offline-not-none
description: the no-network mode is publicly "offline" on the flag, in COOP_EGRESS and in project.yaml; "none" is internal only and refused, not aliased
scope: cli-grammar
sources: [internal/egress/policy.go, internal/config/config.go, internal/project/project.go, internal/cli/network_flags.go, internal/cli/build_cmd.go, internal/cli/completion.go, internal/cli/help.go, internal/cli/acp_cmd.go, internal/sessionsvc/acp.go, internal/sessionsvc/network.go, docs/networking.md, docs/configuration.md]
check: "go test ./internal/cli -run 'TestEgressFlagSaysOffline|TestHelpSpellsTheNoNetworkModeOffline'"
updated: 2026-11-03
---

# Say offline for no network; none is internal only

Every surface a person reads or types says `offline`: `--egress`, `COOP_EGRESS`,
`box.egress`, help, completion, messages and docs. `none` is refused with a message that names
`offline`; it is never an alias. Parse a person's word with `egress.ParseWord` (config's
`parseEgress` is the leaf-package twin), and render a mode with `Mode.Word()`, including for a
child coop that parses its own `--egress` or `COOP_EGRESS`.

**Internal code is exempt:** `egress.None`, `cfg.Egress == "none"`, snapshots, approvals,
execution records and the session API and controller JobSpec keep `none` (`egress.ParseMode`).
Renaming stored or wire values would need a migration for no user value. The boundary is what a
person types or reads.

**Why:** the command line accepted only `none` while `.agent/project.yaml` accepted only
`offline`, so each refused the other's word. The owner chose `offline` everywhere on
2026-11-03 ("Go and fix everything!", task 2026-11-03-use-one-word-for-the-no-network-mode).

**How to apply:** a new launch flag, setting or message takes the mode through `ParseWord` and
prints it through `Word()`. A process that hands a mode to another coop must render the word, or
the child refuses to start. See [[credentials-not-profiles]] for the same retire-not-alias shape.

## Changelog
- 2026-11-03 — created with the rename. Swept every `--egress`/`COOP_EGRESS` parser and child
  writer (flag, build flag, config, project file, fork loop flags, `spawnBox`,
  `sessionACPChildEnvironment`, `networkChildEnvironment`), help, completion, messages, authored
  docs and the scripted suites' environments: all say `offline` now. Stored and wire values keep
  `none`.
