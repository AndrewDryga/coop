---
name: box-logins-device-code
description: "boxed agent logins use device-code/paste flows; browser OAuth hangs in a container"
scope: box
sources: [internal/agent/codex.go, internal/agent/claude.go, internal/agent/gemini.go, internal/agent/grok.go]
check: "none"
updated: 2026-08-09
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
- **gemini** — logs in on first interactive use (Google OAuth). If that ever
  hangs in the box, switch it to a device / no-browser flow too.
- **grok** — `coop login grok` runs `grok login --device-auth`, same shape as codex.

**Why:** a container can't open a browser or receive a localhost OAuth redirect.

**How to apply:** for any new boxed agent login, prefer a device-code / paste-token
flow over browser OAuth. Not mechanically lint-checkable, so it lives here.

## Changelog
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
