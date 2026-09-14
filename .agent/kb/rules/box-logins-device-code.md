---
name: box-logins-device-code
description: "boxed agent logins use device-code/paste flows; browser OAuth hangs in a container"
scope: box
sources: [internal/agent/codex.go, internal/agent/claude.go, internal/agent/gemini.go, internal/agent/grok.go, internal/box/run.go, internal/box/login_test.go, internal/box/filtered.go, internal/box/derived_image.go, internal/box/network_bundles.go, internal/box/network_bundles_test.go, internal/cli/commands.go, internal/cli/commands_test.go, internal/cli/launch_box.go]
check: "none"
updated: 2026-09-14
---

# Agent logins in the box use device-code flows, not browser OAuth

The box is a headless container: it has no browser, and an agent's default
localhost OAuth redirect can't reach the host browser — so browser-based login
hangs.

- **codex** — `coop login codex` runs `codex login --device-auth` (prints a URL +
  code to open on any device). Plain `codex login` (browser/localhost redirect)
  hangs in the box.
- **claude** — `coop login claude` runs `claude auth login`; Claude Code's sign-in is a
  paste-a-code flow, not a localhost redirect, so it works in the box — and unlike a bare
  `claude` it re-authenticates even when you're already logged in.
- **gemini** — first-use interactive login uses `NO_BROWSER=true`, which selects the native
  manual Google flow. Keep its selected account's user settings writable during login: Google
  selection saves `security.auth.selectedType` before relaunching. Managed defaults and the empty
  MCP allowlist belong in a separate immutable system config, not over that writable file.
  An already signed-in account opens normally; use native `/auth` to explicitly sign in again.
- **grok** — `coop login grok` runs `grok login --device-auth`, same shape as codex.

**Why:** a container can't open a browser or receive a localhost OAuth redirect.

**How to apply:** for any new boxed agent login, prefer a device-code / paste-token
flow over browser OAuth. Not mechanically lint-checkable, so it lives here.

Sign-in must not depend on a project's database, MCP servers, instructions or preset. Do not mount
the project into the login cwd, build its image, publish its development ports or start its services. Preserve
the selected credential's writable storage and network admission; ordinary coding config overlays
remain read-only. `TestLoginOnlyMountsSelectedCredentialAndManagedSettings` pins this for every
registered provider; native qualification must also exercise settings persistence across a restart.
Use a container-only scratch cwd, not HOME, so native project warnings do not misdescribe login.
A filtered capture with service grants cannot run without service bindings: refuse it before
runtime setup rather than starting services or silently changing the admitted policy. The operator
can sign in from a project with a service-free policy; never change network posture automatically.

## Changelog
- 2026-09-14 — fixed the ordinary-network login path to select the shared client image instead of
  a project's toolchain image. Reproduced with Blitz Infra's image, which had Claude and Gemini
  but no Grok. Also stopped filtered admission from requiring Grok's old credential before its
  login can replace it. Focused CLI and network-bundle regressions cover both failures.
- 2026-09-13 — reproduced Gemini 0.59's failed-save/relaunch loop and rejected MCP field. Swept all
  four login adapters and the shared launch path: isolated login from project setup, retained
  managed defaults and native manual auth, and pinned the selected writable home without repo mounts.
  Board sweep also caught and pinned the ordinary stale-image rebuild and filtered project-image /
  service-grant paths; login skips builds and refuses service-bearing filtered captures before setup.
- 2026-06-14 — created
- 2026-06-17 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: read every agent's `Login()` in internal/agent/{claude,
  codex,gemini,grok}.go. 0 violations — claude (`claude auth login`, claude.go:107), codex
  (`codex login --device-auth`, codex.go:119), and gemini (bare `gemini`, first-use OAuth,
  gemini.go:188) match the card exactly. 1 coverage gap (not a code violation): grok.go:160-161
  also correctly implements `grok login --device-auth`, but grok isn't mentioned anywhere in the
  card's per-agent list — it was added after this card's last revision. Flagged for the lead as a
  card update (add the grok bullet), not fixed here.
- 2026-08-09 — drift repair from the backfill sweep's findings: grok bullet added (grok login --device-auth, grok.go); sources widened to all four adapters.
