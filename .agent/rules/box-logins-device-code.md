# Agent logins in the box use device-code flows, not browser OAuth

The box is a headless container: it has no browser, and an agent's default
localhost OAuth redirect can't reach the host browser — so browser-based login
hangs.

- **codex** — `coop login codex` runs `codex login --device-auth` (prints a URL +
  code to open on any device). Plain `codex login` (browser/localhost redirect)
  hangs in the box.
- **claude** — `coop login claude` works as-is: Claude Code's login shows a
  paste-a-code flow, not a localhost redirect.
- **gemini** — logs in on first interactive use (Google OAuth). If that ever
  hangs in the box, switch it to a device / no-browser flow too.

**Why:** a container can't open a browser or receive a localhost OAuth redirect.

**How to apply:** for any new boxed agent login, prefer a device-code / paste-token
flow over browser OAuth. Not mechanically lint-checkable, so it lives here.
