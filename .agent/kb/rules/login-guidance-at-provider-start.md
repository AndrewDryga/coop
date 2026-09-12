---
name: login-guidance-at-provider-start
description: Put sign-in guidance directly after Starting and separate confirmed login success from box shutdown
scope: cli-output
sources: [internal/cli/commands.go, internal/cli/login_output_test.go, internal/box/launch_sections.go, internal/box/launch_sections_test.go]
check: go test ./internal/cli -run TestLoginNarration
updated: 2026-09-12
---

# Put sign-in guidance beside the provider that is about to start

Boxed login starts with `Signing in to <target>` and a blank line. Normal secret/network setup
follows. Immediately below `Starting <provider>`, print `Follow <provider>'s sign-in instructions
below` without a period. Then leave the provider's own output untouched. Separate the confirmed
box-stop sentence from `✓ Signed in to <provider>` with a blank line, preserving the Start action.

**Why:** the user supplied this exact ordering and spacing correction for `coop login grok` on
2026-09-12. Early guidance appeared before setup rather than next to the interactive sign-in.

**How to apply:** use the existing shared launch narration boundary, not a new callback that
changes watchdog timing. Guidance is login-only and stays silent in batch/quiet/ACP output.
Keep readiness checks: exit zero alone is still not proof of successful sign-in.

## Changelog
- 2026-09-12 — swept login handoff/result and all three Starting call sites (normal, filtered,
  restricted). Shared presentation data now places guidance correctly; exact Grok launch/handoff/
  success tests and existing Claude approval fixtures cover the correction. No provider auth changed.
