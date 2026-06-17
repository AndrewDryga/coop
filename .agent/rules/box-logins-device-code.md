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

**Why:** a container can't open a browser or receive a localhost OAuth redirect.

**How to apply:** for any new boxed agent login, prefer a device-code / paste-token
flow over browser OAuth. Not mechanically lint-checkable, so it lives here.
