---
name: provider-client-qualification
description: every box runs one locked client set; make provider-qualify records a strict live qualification (no record or gate yet — why) — the conformance rows, where each is proven, the gaps, the bump procedure
subsystem: agent
sources: [internal/agent/locked_clients.go, internal/agent/locked-clients/package.json, internal/agent/locked-clients/package-lock.json, internal/box/locked_image.go, internal/box/image.go, tools/qualify/main.go, Makefile, internal/cli/provider_live_e2e_test.go, internal/acpproxy/e2e_test.go, internal/box/credential_broker_test.go]
updated: 2026-09-19
---

**One manifest.** `locked-clients/package.json` + `package-lock.json` (embedded) and each adapter's
`LockedClients` (npm packages, or a native artifact with its SHA-256) are the only client
declaration. `lockedClientParts` renders the one client layer both images install — the base
(`baseImageDefinition`) and the filtered client image (`lockedImageDefinition`) — so ordinary,
filtered, login, loop, preset, consult/delegate and ACP boxes run the same bytes. Launchers live in
`agents.LauncherDir` (`/opt/coop/bin`), first on PATH, so an asdf shim cannot shadow a client; they
exec absolute paths on the image's own Node. Each adapter's `UpdateControls` switch its updater
off in every image; the environment ones (Claude, Grok) also ride BoxEnv, and the home overlays
cover Codex and Gemini in images Coop did not build. A project image built on
`COOP_BASE_IMAGE` inherits all of it; one on another base brings its own, unqualified clients.

**The record.** `make provider-qualify` (PAID) rebuilds this host's box and filtered setup from the
working tree, runs every strict live suite (plus a model+effort run), and `tools/qualify` writes
`locked-clients/qualification.json` only when each summary is strict, every provider passed once,
and each reported `<provider>-cli <semver>` equals the pin. It is keyed by `QualifiedClientSet()`:
the lock's SHA-256 plus, per platform, each client's provider, kind, package/binary, version,
required executables' versions and native digest (paths stay out — moving a launcher is not a new
client). **No record exists yet, so no gate enforces one:** on 2026-09-19 the strict suites could
not pass on any host: Gemini's only portable account family is an API key, which needs filtered
networking, while the open, consult and ACP suites run open (Gemini OAuth is host-bound). The
broker now serves a key to a consult peer, an ACP session and a remote session on filtered
networking; routing API-key targets through the filtered gateway in every suite, the first record
and the gate that fails a pin move without a matching one land with the follow-up task
(`2026-09-19-close-the-live-conformance-gaps-in-provider-qual`).

**Conformance rows** (D = deterministic in `make check`, L = the paid run; all four providers unless
noted — mapped 2026-09-19 by reading the tests):
- start — D `TestProviderScriptedProcessSmoke`, `TestProviderScriptedDirectMatrix`, ACP switch matrix;
  L `provider-live-e2e-all`, `acp-e2e`.
- resume — D `TestResume`, fork session process, ACP target replay, consult continuity;
  L `provider-resume-live-e2e-all`, `provider-network-live-e2e-all`.
- cancel — D DirectMatrix "cancellation", `TestScriptedACPCancelAndContinue`; L `acp-e2e`.
- model + effort — D DirectMatrix argv/env, loop lifecycle matrix; L `provider-live-e2e-effort`
  (`tools/qualify -targets`: each `ExampleModel` at high effort).
- skills — D `TestSynthSkillsMounts` (mount plan only). **No process-level or live proof.**
- MCP — D per-route unit tests (claude's `--strict-mcp-config` argv among them);
  L `provider-loop-live-e2e-all` (Coop's task tools). **The user's shared MCP servers are never
  exercised live, and no process-level test asserts them.**
- tool lifecycle — D `loop/streamjson_activity_test.go` per provider; watchdog process test for
  claude/codex/grok (**not gemini**). **No live decoding.**
- quota classification — D pinned ACP signals, ACP rate-limit recovery with each provider's real
  shape (loop/consult/delegate fixtures print generic text). **No live proof** (a real limit
  cannot be triggered on demand; the shapes are captured from the pinned clients).
- helper discovery — D consult/delegate/preset/native-role matrices; L `native-roles-e2e`,
  `provider-consult-live-e2e-all` (wrapper called directly). **No live delegate proof.**
- account switching — D DirectMatrix account selection, ACP rotation. **No live proof.**
- clean completion — D loop lifecycle matrix; L `provider-loop-live-e2e-all`.

**Bumping a client.** Edit `package.json` and regenerate the lock (`npm install
--package-lock-only --ignore-scripts --registry=https://registry.npmjs.org` in `locked-clients/`);
move the adapter's `LockedClients` (versions, platform paths; Grok's URL and both digests) and any
`NetworkBundle` source URLs; re-read what the pins encode (`TestGeminiThinkingIsQualifiedOnTheLockedClient`,
the captured `login-failures` and `grok-*-tools.jsonl` fixtures); then run `make provider-qualify`
and commit the record with the bump. `validateClientClosure` also pins Playwright's version.

A brokered client's request shape is pinned too: `TestCredentialBrokerRoutesAdmitWhatThePinnedClientsSend`
holds the request line each client version really sends (a synthetic fixture once hid that Claude
posts `/v1/messages?beta=true`, so every brokered Claude request was refused). Re-capture on a bump,
offline: run the client in the locked image with `--network none`, a tiny local listener that
logs `method url` and answers 403, and the client's base URL pointed at it — Claude
`ANTHROPIC_BASE_URL` (and claude-agent-acp's bundled SDK `claude` directly), Gemini
`GOOGLE_GEMINI_BASE_URL` with `settings.json` selecting `gemini-api-key` and
`GEMINI_CLI_TRUST_WORKSPACE=true`, Codex its managed config's `base_url`. Also re-verify that the
pinned codex still loads `/etc/codex/managed_config.toml` over `-c`, project and user config: the
broker's Codex provider rides that file. MCP routes are pinned the same way
(`TestMCPRoutesAdmitWhatThePinnedClientsSend`): point each client (and claude-agent-acp's bundled SDK
`claude`) at a local endpoint that answers `initialize`/`tools/list` and logs `method url` plus the
Authorization shape — claude `--mcp-config` with `headers.Authorization: "Bearer ${VAR}"` (it ignores
`bearer_token_env_var`), codex `config.toml`, gemini `settings.json`, grok `config.toml` — and move
the pins. 2026-09-19: every client sends POST (initialize, notifications, tools) and GET (the stream)
to the exact path with the bearer.

Traps: the strict suites fail on any skip, so every provider needs a signed-in default account whose
access token outlives the run (a Claude or Grok token hours old is skipped as refresh-required —
one real prompt refreshes it); `provider-qualify`'s preflight refuses `COOP_IMAGE` (it would
qualify a foreign image) and a provider with no default account; a changed client layer makes every
host re-run filtered setup once — a filtered launch does it itself, an editor session refuses until
`coop net setup`.

## Changelog
- 2026-09-19 — added the MCP request-line pins and their capture.
- 2026-09-19 — the broker serves consult peers, ACP and remote sessions on filtered networking;
  added the request-line capture to the bump procedure.
- 2026-09-19 — created with the one-manifest base image, update controls, the qualification tooling
  and the row map. The first run stopped on Gemini (a brokered API key cannot run the open, consult
  or ACP suites); the first record, its gate and the missing live proof are queued as one task.
