---
name: provider-client-qualification
description: locked clients, strict schema2 qualification requirements and Linux/amd64 live evidence
subsystem: agent
sources: [internal/agent/locked_clients.go, internal/agent/qualification.go, internal/agent/qualification_gate_test.go, internal/agent/locked-clients/package.json, internal/agent/locked-clients/package-lock.json, internal/box/locked_image.go, internal/box/image.go, internal/box/gitenv.go, tools/qualify/main.go, Makefile, internal/cli/provider_live_e2e_test.go, internal/cli/provider_loop_live_e2e_test.go, internal/cli/provider_resume_live_e2e_test.go, internal/cli/provider_network_live_e2e_test.go, internal/cli/provider_consult_live_e2e_test.go, internal/loop/provider_accounts_live_e2e_test.go, internal/acpproxy/e2e_test.go, internal/box/credential_broker_test.go, internal/box/locked_client_fixture_e2e_test.go, internal/box/skills_runtime_e2e_test.go, internal/box/native_roles_runtime_e2e_test.go, internal/box/mcp_runtime_e2e_test.go, internal/testutil/liveprovider/contract.go, internal/testutil/liveprovider/credentials.go]
updated: 2026-10-01
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

The base image stages its embedded files under a private temporary root. `COPY system/ /`
imports source directory and file modes, so the entries *inside* that root must be reset to
0755 directories and 0644 files after staging: under `umask 077`, inheriting 0700 for
`system/etc` makes `/etc` root-only in a non-root box. Keep the outer root 0700. The
filtered client image uses a separate tar context whose file modes are explicit. The
staging regression runs under a private umask. A rebuilt Ubuntu image confirmed
`/etc` is 0755 and Codex, Claude and Grok launch as a non-root user.

A live qualification child running a brokered API key must receive the host's
`NetworkStateHome`, so `ChildEnvironment` selects the filtered credential gateway.
This applies to both native-resume stages, not only the ordinary prompt suite;
signed-in accounts keep their open path. The selected credential kind is determined
from the real host config before the isolated child starts.

**The record.** `make provider-qualify` calls real models, last after other engineering.
Included subscription access needs no additional spend permission; unknown API-key billing or
paid overage does. Credentials being present does not prove an account's billing allowance.
It rebuilds this host's images, runs offline probes first and every required live suite, then
atomically writes `locked-clients/qualification.json`. `QualificationRequirements` is the single
schema2 suite/provider/evidence map used by the recorder and `TestTheLockedClientsMatchTheirQualification`.
Every CLI row must report the exact pinned version; test-only rows must pass their stated scope.
Only second-account recovery permits `not_configured` (fewer than two configured accounts), never
a pass implied by missing prerequisites. Unreadable account catalogs fail, and the recorder's
success output names any providers whose account recovery remains unverified. Existing records
must have complete exact scopes and match `QualifiedClientSet()` (lock SHA-256 and every platform's
client/native artifact identity).
Malformed, duplicate, non-strict, failed-footer or incomplete summaries cannot produce a record.
The 2026-09-26 Linux/amd64 record was produced from strict pinned-client suite logs: offline
roles/skills/MCP, direct prompt, native resume/loop/consult/delegate, filtered networking,
ACP adapters and the model+effort probe. `make provider-qualify` reached ACP but stopped on a
stale Frontier test expectation after its preset lead changed; a one-line test repair and a
complete green ACP rerun supplied that row. The production source was unchanged between the
earlier green suites and that replay. The recorder validated all required logs and pins before
writing the record. This is not a claim that one uninterrupted `make provider-qualify`
invocation passed, or that macOS/Linux-arm64 live provider parity was exercised. Only one
account was configured per provider, so all four second-account recovery entries remain
`not_configured`, not proved. A missing-file gate is dormant, not proof of compatibility.

**Temporary exact-client gate policy.** The v10.1.2 gate policy hashes both the
lock and every platform's complete client/native identity, and requires 10.1.2 to be the
latest numbered changelog section. Reopening Unreleased permits ordinary main checks; the
next release or any client/dependency change refuses. Release preflight independently binds
the finalized first section to the actual event tag. No environment bypass exists. The
validator and historical record are unchanged. Remove
the temporary exception after this release; it is not live compatibility evidence.

Live harnesses route brokered API keys through the host's filtered gateway automatically, using
`BrokersKey`/`AnyBrokersKey` and `NetworkStateHome`; native sign-ins retain the open path except
the explicitly filtered network suite. One brokered key filters the entire consult/delegate ring.
Prepare projects selected host-vault keys outside mounted homes, honors default-env precedence,
and fingerprints only selected authority. Gemini OAuth remains host-bound. Historical proof on
2026-09-20 covered Gemini's basic prompt and singleton ACP conformance, not all suites or current
credentials. `NetworkStateHome` lets the child select filtered mode, but each direct consult/delegate
`box.Run` (version probe and peer edge) must separately admit the actual launch with
`box.AdmitNetwork` and pass its `CapturedEgress`. The edge wrapper is an agent command for the
broker's credential boundary. Filtered launches cannot use `--cidfile`; supervisor-label cleanup
still owns them. Mark an attempt only after admission, so an admission failure is not reported as
a model run. Isolated ACP homes link Docker CLI plugins so their filtered setup can find Buildx;
filtered launches admit the supervisor's `--label`.

The filtered Grok live suite must renew a selected, renewable trusted source login *before*
the harness fingerprints and projects it. Its child receives only an access token, so Grok's
one-hour restricted admission needs that copy to remain valid through the bounded child run.
The network suite checks the admission horizon plus its child window; a login without refresh
authority retains a `credential_refresh_required` prerequisite skip. This is harness setup,
not a relaxation of the product's admission horizon or permission to copy refresh tokens.

A filtered singleton's toolbar has NO Preset dropdown when the repo's only preset needs another
provider: the supervisor freezes the scope to the one brokered provider (`LimitNetworkTargets`), so
`networkPresetAllowed` filters a codex-led preset out, its selector collapses to sole-"none", and
`visibleConfigOptions` hides it. Correct, and the ACP conformance test now expects it for a filtered
session (an OPEN singleton still shows every repo preset, unfiltered by lead capability).

**Conformance rows** (D = deterministic in `make check`; R = the real pinned client, run OFFLINE in
the locked image with `--network none` — free, but outside `make check` because it needs the image
`coop net setup` builds. An R suite gates `provider-qualify` before model calls and enters
the record with its explicit provider scope. L = a live assertion implemented in the harness,
not a claim that it has run successfully; all four providers except unavailable second accounts):

The three R probes bind-mount synthetic files from `t.TempDir()` into the locked client image,
which runs as `node`. Go keeps the enclosing test directory private; under host umask 077, the
returned bind-source leaf and nested fixture paths also become unreadable to `node` on direct
Linux Docker. Make the entire synthetic source tree traversable/readable only after writing it
(`readableLockedClientFixture`), while keeping the enclosing test directory private, the bind
read-only and the client unprivileged. A Mac pass at umask 022 does not prove Linux/umask 077.

- start — D `TestProviderScriptedProcessSmoke`, `TestProviderScriptedDirectMatrix`, ACP switch matrix;
  L `provider-live-e2e-all`, `acp-e2e`.
- resume — D `TestResume`, fork session process, ACP target replay, consult continuity;
  L `provider-resume-live-e2e-all`, `provider-network-live-e2e-all`.
- cancel — D DirectMatrix "cancellation", `TestScriptedACPCancelAndContinue`; L `acp-e2e`.
- model + effort — D DirectMatrix argv/env, loop lifecycle matrix; L `provider-live-e2e-effort`
  (`tools/qualify -targets`: each `ExampleModel` at high effort).
- skills — D `TestSynthSkillsMounts` (the mount plan) AND
  `TestProviderScriptedSharedSkillsReachEveryCapableClient` (process level, per provider): the repo's
  shared `.agent/skills` reaches each skills-capable client at its OWN `$HOME/.<provider>/skills`,
  writable, from a synthesized copy — and is absent for a client that does not discover skills. The
  provider fixture now pins that shape (`validateSkillsMount`), so the mount cannot change silently.
  R `skills-e2e` closes most of the other half — that each client reads that directory — by asking
  the client itself, offline: codex `skills/list` over its app-server stdio JSON-RPC, `gemini skills
  list` and `grok inspect --json` all NAME the skill, so those three are real discovery proofs.
  Claude is weaker and labelled so: it has no offline command that reports loaded skills (`claude
  skills list` needs an account), so the row runs `plugin validate ~/.claude --json --strict`, which
  proves its tooling finds and parses `skills/` at the mounted layout, not that its runtime loads it.
  Two guards keep a row from passing for the wrong reason: the probe runs from a working directory
  that is NOT the home, because all of these clients also discover `<cwd>/.<agent>/skills` as a
  project skill and report the same absolute path (a probe run from `$HOME` would keep passing for a
  client that dropped the user-level root — the regression the test exists to catch); and a decoy at
  `~/.<agent>/notskills` must appear in NO report, written in the shape that client would report
  (frontmatter-less for claude, or the guard rules out nothing). Move the real skill off the mounted
  path and all four rows fail.
- MCP — D per-route unit tests (claude's `--strict-mcp-config` argv among them) AND
  `TestProviderScriptedSharedMCPReachesEveryClient` (process level, per provider): an operator's
  shared MCP file reaches every client, read-only, at a path that client reads. Asserted on the
  SERVER NAMES the generated config carries, which the provider fixture captures at launch time —
  coop deletes those files when the run ends, so a test that looked afterwards would find nothing.
  L `provider-loop-live-e2e-all` (Coop's task tools) and `provider-network-live-e2e-all` (mandatory
  controlled shared stdio tool call, all providers). R `mcp-e2e` proves the client then REACHES the
  server, for codex, gemini and grok: a stdio server is a child process, not a network peer, so this
  runs offline. The witness is the server itself (`internal/box/testdata/mcpprobe` logs every
  JSON-RPC method it receives). What separates a real handshake from an announced one is ORDER, not
  presence: the probe sits on its initialize RESULT and logs `answered initialize` only after writing
  it; a concurrent reader timestamps receipt, so a pipelined `notifications/initialized` lands
  before that line and fails. This proves response ordering, not the client's private read state.
  Presence alone proves nothing — a client could write both back to back, which is
  what the codex driver here does deliberately. The configs come from Coop's own adapters
  (`agents.Agent.MCP`), not hand-written, and codex connects from NO `codex mcp` subcommand — `mcp
  list` prints config without launching anything; its app-server's `mcpServerStatus/list` launches
  them. The failure path is IN the suite: each row re-runs with an empty `mcpServers` and nothing may
  start. One control run by hand, worth knowing: flipping `security.folderTrust.enabled` back to true
  fails gemini ALONE, so that generated setting is the only thing keeping an operator's servers from
  being silently suppressed in a box's untrusted folder, and the gemini row is its standing test.
  **Claude's connection is still LIVE-only and cannot be probed offline: Coop hands it servers with
  `--mcp-config` on the main invocation; `claude mcp list` rejects that flag outright, accepts and
  ignores it before the subcommand, and otherwise reads the `mcpServers` key of its own user config.
  Coop DOES write that file (onboarding, bypass and trust keys, under CLAUDE_CONFIG_DIR) but never
  writes servers into it.** Its actual shared-tool call is required by the live network suite.
  That witness sits in the isolated provider profile and is cooperative compatibility evidence,
  not model-resistant attestation. Missing, wrong-tool, duplicate and cross-process witnesses fail.
- tool lifecycle — D `loop/streamjson_activity_test.go` per provider; watchdog process test now
  covers a silent start for claude, gemini and grok (`TestProviderScriptedLoopWatchdogProcess`,
  "no first output from <provider> rotates and completes") plus grok's foreground-tool and tool-cap
  cases — a start timeout is read from each client's OWN first-output shape, so one provider going
  quiet proves nothing about another. L `provider-loop-live-e2e-all` now feeds native output
  through production `IterationCommand` and the production lifecycle decoder, requiring paired
  tool events and exactly one successful terminal event, independent of task-MCP completion.
- quota classification — D pinned ACP signals per provider (claude `errorKind=rate_limit`, codex
  `usageLimitExceeded`, gemini `RESOURCE_EXHAUSTED`, grok `http_status=402`), ACP rate-limit recovery,
  AND `internal/ladder/limit_test.go` over REAL captured prose: claude's weekly subscription limit,
  codex's usage-limit and model-at-capacity notices, gemini's output limit and max-tokens finish
  reason, plus negatives (a 429 inside a larger number, a `codexErrorInfo` field NAME alone).
  The loop fixtures printing generic text is CORRECT, not a gap: `ladder.DetectIterationLimit` is
  provider-agnostic prose matching, so the fixture proves the loop's REACTION (rotate account, then
  provider) while the ladder corpus proves DETECTION against what the clients really emit.
  **Open: no captured loop-path sample for grok's credits exhaustion — its 402 is pinned only on the
  ACP path, and its 429 reads as "Rate limited", which the broad keyword case covers. No live proof:
  a real limit cannot be triggered on demand.**
- helper discovery — D consult/delegate/preset/native-role matrices; R `native-roles-e2e`
  (Codex checks its user-level role directory through rejected parser canaries, Gemini counts one
  additional loaded role, and Grok reports the role by name. These probes run outside the home so
  project scope cannot answer for it. Claude's `plugin validate` is handed its role directory
  explicitly: it proves syntax, not automatic user-level discovery.
  No paid call; Gemini's row uses a dummy key and reads the debug line before failure);
  L `provider-consult-live-e2e-all`
  (wrapper called directly), plus `provider-delegate-live-e2e-all`: the same four-edge ring in
  delegate mode, exactly one requested file, no extra/ignored/staged/committed mutations.
- account switching — D DirectMatrix account selection, ACP rotation, AND
  `TestProviderScriptedLoopRotatesAccountsBeforeProviders` (process level, every provider): a limit
  on one account rotates to the SAME provider's second account before any other provider, and each
  hop is recorded against the account that actually hit it. The order is the claim — reverse the
  ladder and it fails. Loop-path rotation was claude-only before, proven through its structured
  credit-limit stream, which left the ORDER untested for the other three.
  L `provider-accounts-live-e2e-all`: real controller, two distinct configured compatible accounts,
  invalid synthetic first copy then unchanged second copy, at most two work launches. Strict
  telemetry must show authentication then success on the expected accounts; one exact committed
  task must finish. Fewer than two is `not_configured`; expired/unsafe/incompatible pairs fail.
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
to the exact path with the bearer. Header secrets are pinned beside them: servers with
`X-Api-Key: ${VAR}` and `X-Auth: Token ${VAR}` rendered by Coop's own renderers (Codex takes only the
first; Coop refuses text before a reference for it), and every client sends the header's text with
the value in place. Capture grok with a fake `XAI_API_KEY` and `-p`, not `grok mcp doctor`, which
probes OAuth discovery paths and GETs the endpoint without the configured headers.

Traps: the strict suites fail on any skip, so every provider needs a signed-in default account whose
access token outlives the run (a Claude or Grok token hours old is skipped as refresh-required —
one real prompt refreshes it); `provider-qualify`'s preflight refuses `COOP_IMAGE` (it would
qualify a foreign image) and a provider with no default credential, using the ordinary presence
policy so an env-only account needs no directory; a changed client layer makes every
host re-run filtered setup once — a filtered launch does it itself, an editor session refuses until
`coop net setup`.

The live loop verifier snapshots Git administrative state before the box commits. A different
box umask can change permissions on Git's rewritten ref and index files; Coop's
`prepare-commit-msg` hook must preserve `COMMIT_EDITMSG`'s original private mode too. The
verifier tolerates safe permission-bit drift on allowlisted mutable Git files but rejects
executable or world-writable modes, retaining their paths/types and exact semantic commit,
reflog and index checks.

## Changelog
- 2026-10-01 — documented the v10.1.2-only exact-client gate policy; release copy remains
  product-focused and decision history stays in local task records.
  Focused positive/changed-client/changed-release checks pass; historical evidence and the
  strict evidence validator remain unchanged. Fresh accounts/ACP coverage is still incomplete.
- 2026-09-30 — pre-release refresh recaptured actual HTTP/MCP shapes on new clients, including
  managed Codex provider precedence and Gemini system-file ownership/effort behavior, offline
  with synthetic credentials. New full schema2 live qualification remains pending; retained the
  old record unchanged. Clarified account-billing permission rather than assuming every call is paid.
- 2026-09-29 — rechecked native-role and skill/MCP offline probes against their actual commands.
  Claude's explicit role-file validation is not automatic discovery; its skill validator proves
  layout/parsing only, and its shared MCP route remains live-only. Updated test/Make labels without
  changing the probe behavior or claiming new live proof.
- 2026-09-28 — narrowed the first brokered-live changelog claim to its actually proven prompt
  and singleton ACP paths; added the live-provider helpers to this card's source map.
- 2026-09-26 — recorded strict Linux/amd64 qualification from complete green suite logs;
  documented the ACP test-only replay and unverified second-account recovery
- 2026-09-26 — corrected the consult/delegate filtered-launch boundary after the clean Ubuntu
  qualifier passed direct, resume and loop suites but all four consult version boxes refused
  missing host-policy capture before model launch. Verified the per-launch repair with strict
  consult and delegate 4/4 replays on the same pinned clients; no record claimed yet.
- 2026-09-25 — a real Grok filtered-network qualifier failed before launch with a
  31-minute projected token: the harness preflight checked 30 minutes, but the
  child required one hour. Renewing the trusted source before the access-only
  snapshot and aligning the horizon passed a bounded Grok 1.0.25 live replay;
  no-refresh and renewal-denial controls protect the existing boundaries.
- 2026-09-25 — strict Ubuntu loop qualification exposed a shared Git-admin mismatch after all
  four providers completed their tasks. The real hook reproduced a `COMMIT_EDITMSG` mode change
  from 0600 to 0644 under differing umasks; preserve the mode in its temporary replacement.
  Plain Git changed a ref and index from 0600 to 0644 under the same umask transition on Mac,
  and the ref on Ubuntu; normalize safe mutable-file permission drift in the verifier. Review
  caught world-writable/executable permissions that the first normalization would accept;
  focused denials now reject both. Live loop replay and qualification remain pending.
- 2026-09-25 — real Ubuntu qualification passed four direct prompts but Gemini
  API-key native resume failed before grading because both resume children omitted
  the filtered host-state grant. Routed the selected brokered key through that
  gateway in fresh and continued stages; the pinned Gemini 0.59.0 live resume
  probe attempted and passed. Full multi-suite record remains pending.
- 2026-09-25 — strict VM qualification exposed a real root-only `/etc` in a base image built
  under umask 077: Codex's Node could not read OpenSSL config (exit 13), while Claude/Grok
  prompts exited. Traced `COPY system/ /` to the base embedded context's inherited modes;
  added explicit staged modes and a private-umask regression. Distinct from the earlier
  locked-client fixture bind-source issue below. The rebuilt non-root image and strict
  four-provider prompt suite passed; the full qualification record remains pending.
- 2026-09-24 — direct Ubuntu qualification under umask 077 exposed a private `t.TempDir` bind
  source unreadable by the locked image's `node` user; nested synthetic content stayed private too.
  the same VM source passed at umask 022. Made each generated fixture tree readable after writing,
  kept the enclosing test directory private and mounts read-only. Explicit campaign spending
  authorization superseded the earlier operator-only wording, not a general unattended paid policy.
- 2026-09-22 — closed harness gaps for host-vault keys, mandatory shared MCP, native lifecycle,
  delegate and real-controller account recovery; schema2 recorder and gate share requirements.
  Synthetic controls are not paid proof; record remains absent and operator qualification is last.
- 2026-09-21 — the consult ring routes a brokered key through the gateway; its "run the agent
  itself" deferral was a misreading, and the real gap was a missing NetworkStateHome on
  ConsultChildSpec. Live proof still owed by the paid run.
- 2026-09-21 — the recorder learned per-suite provider scope, so `skills-e2e`, `mcp-e2e` and
  `native-roles-e2e` now enter the record; `mcp-e2e` carries claude's omission as part of it.
- 2026-09-21 — `native-roles-e2e` probes from outside the home too; its control shows grok reads
  project scope as well as user scope, the other three only user scope.
- 2026-09-21 — the shared-MCP row's connection gap closed OFFLINE for codex, gemini and grok
  (`mcp-e2e`), asserted from the server's side; claude cannot be probed offline and is recorded as
  the remaining live gap with the evidence, so nobody re-derives it.
- 2026-09-21 — the skills row's live gap closed OFFLINE: `skills-e2e` asks each pinned client what it
  loaded from `~/.<agent>/skills`, no model call. Split the row legend (R) from the paid run, because
  `native-roles-e2e` was filed under L while making no paid call. Both offline suites now run FIRST
  in `provider-qualify`, so a red there cannot arrive after every provider has answered paid prompts. Two non-obvious findings: codex
  exposes its skill catalog only to the model (`skills.list`), but its app-server answers the same
  `skills/list` over stdio JSON-RPC, and claude's validator reports ONLY what it objects to — a valid
  skill leaves `contents: []`, identical to an empty directory, so a canary is the only usable signal.
- 2026-09-20 — prompt and singleton ACP live harnesses routed brokered API-key targets through the
  filtered gateway (BrokersKey/AnyBrokersKey); Gemini proven live on provider-live-e2e and ACP
  conformance. Recorded
  the filtered-singleton toolbar (no Preset when no in-scope preset), the --label admission and the
  Buildx plugin link an isolated ACP home needs.
- 2026-09-19 — pinned how each client sends a header secret; grok's `mcp doctor` is not a capture.
- 2026-09-19 — added the MCP request-line pins and their capture.
- 2026-09-19 — the broker serves consult peers, ACP and remote sessions on filtered networking;
  added the request-line capture to the bump procedure.
- 2026-09-19 — created with the one-manifest base image, update controls, the qualification tooling
  and the row map. The first run stopped on Gemini (a brokered API key cannot run the open, consult
  or ACP suites); the first record, its gate and the missing live proof are queued as one task.
