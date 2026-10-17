---
name: credential-presence-is-adapter-declared
description: adapters own credential presence, selected env authority, and inspectable stored readiness
subsystem: credentials
sources: [internal/agent/agent.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/acpctl/control.go, internal/box/auth.go, internal/box/credential_broker.go, internal/box/profiles.go, internal/cli/rotation.go, internal/cli/profiles.go, internal/cli/target.go, internal/cli/cli.go, internal/cli/acp_cmd.go, internal/cli/acp_network.go, internal/cli/fork_cmd.go, internal/cli/commands.go, internal/cli/loop_cmd.go, internal/loop/loop.go, internal/loop/review.go, internal/testutil/liveprovider/credentials.go]
updated: 2026-10-17
---

Adapters own six credential facts: `AuthMarker` names their login file and canonical primary env
key; `CredentialEnvKeys` is the complete set of token keys they accept;
`ActiveCredentialEnvKeys` selects the env family authoritative for one profile; and
`StoredCredentialStatus` validates a native marker when its shape is safely inspectable.
`CredentialBroker` declares the one pinned API-key route Coop has actually qualified, while the
optional `StoredAPIKeyDetector` identifies a reusable API key hidden in a native marker so launch
can refuse it instead of mounting it. Presence
callers never reconstruct provider-specific precedence or OAuth schemas. `ProfileAuthed` and
`AuthedAgents` are the public profile and provider views over the shared presence predicate.

The env parser follows the runtime contract: non-empty `KEY=value` is present, and a bare `KEY`
imports the ambient value only when that variable exists. A present bare import or assignment wins;
an unset bare import is omitted and therefore does not clear an earlier assignment. Credential keys
must have one adapter owner because out-of-scope stripping is provider-based. `EffectiveProfiles`
adds an authenticated env-only default even without a profile directory, so `coop credentials`,
loop rotation, peer discovery, ACP defaults, and scoped mounts cannot disagree about
the same login. The env key is provider-wide, so it authenticates exactly the configured default;
other named profiles require their own marker file. Read, completion, runnable-target and account
checks use `EffectiveProfiles`, including the checks before setting a default or removing one. A
run on a marker-backed profile strips that provider's env keys before passing the env file, so the
provider-wide token cannot shadow the account-specific marker mounted into the box. A marker owns
presence when the adapter selects no env authority; the optional `MarkerCredentialSelector`
capability can additionally declare that a native marker stores the same credential as a selected
env family. Environment selection and filtering remain independent. This matters for Gemini:
`gemini-api-key` accepts `GEMINI_API_KEY` from the default account's env or Coop's owner-private
per-account host credential. Gemini's encrypted native marker is bound to the box identity that
created it and does not make an API key portable; `vertex-ai` selects `GOOGLE_API_KEY`, and
`oauth-personal` requires the marker. Named accounts never consume provider-wide env keys, while a
default env key retains the Gemini CLI's environment-first behavior. Because Gemini's shared
encrypted marker is opaque, broad presence remains a best-effort heuristic rather than proof that
its selected entry is usable. Filtered-network admission is stricter: it admits only the exact
account/auth family whose portable authority it can prove and keeps OAuth and not-yet-qualified
Vertex accounts out of the selector.

After presence succeeds, `ProfileCredentialReady` asks the adapter for marker readiness only when
that exact profile has a marker. Claude, Codex, and Grok distinguish usable or refreshable OAuth
from malformed, stripped, and nonrenewable records; Gemini returns unknown and preserves
presence-based behavior. `coop credentials`, runnable ladder expansion, and the ACP Account selector
use this same refinement, so a native login already labeled `re-login required` is neither attempted
during failover nor offered as a normal editor switch. An env-only default likewise remains ready
because there is no native marker to inspect. `ProfileAuthed` stays the broad presence predicate for
callers that cannot require proven readiness, and opaque markers stay eligible because an adapter
cannot safely declare them dead. `ProfileTokenMtime` remains the separate marker-age refinement and
should not grow provider schema or env-key logic. Live projection is a stricter, separate contract:
source refreshability may make a stored login ready, but refresh authority is still omitted from the
projected box credential.

Account checks: `requireProfile` (internal/cli/profiles.go) checks a named account for launches,
preset launches, `coop acp`, `coop fork`, `coop eval` and `coop credentials <agent> <account>`
(show, default, rm). It fails closed: only a listed account passes. A missing account gets
`noAccountErr` and exit 2 (eval exits 1 for any bad configuration). When the listing failed, the
account may be in what could not be read, so it gets `accountsUnlistedErr` instead: "Could not list
every <Agent> account" with the folder and reason, no sign-in advice, exit 1. `reportExit` keeps a
command's own positive code over the error's, so these paths return `refusalExit(err)`
(internal/cli/cli.go), never a flat 2; it looks through wrapping, which a preset's first rung adds.
Not through `requireProfile`: rotation ladders check their rungs' accounts in `expandLadder` with
`ProfileCredentialReady` (`coop loop` and its review stages, preset ladders in fork loops, the lead
expansion behind `coop acp --peer` and filtered editor sessions). A ladder that runs rotates over
the accounts that could be listed. One that resolves nothing names the first of its providers whose
listing failed with `accountsUnlistedErr`, ahead of any sign-in repair, since the account it needs
may be in what could not be read. Accepted trade-off: when a pinned login needs re-login and a
folder cannot be listed, the folder message comes first and the re-login only once the folder lists
again. The advice is late, never wrong. Every command that prints a ladder refusal exits 1, for a
failed listing and a missing login alike: `coop loop`, a fork loop's rotation (`runForkLoop`, and
the `--peer` check in `forkCreate`, fork_cmd.go), the loop's review stages (`Control.Run`,
internal/loop/loop.go) and the lead expansion behind `coop acp --peer` return -1, which takes the
error's code. A `--peer` provider with no usable login gets `needsAccountErr` (target.go), a
`ui.CommandFailed`, so every peer check (launches, `coop loop`, `coop acp`, `coop fork`) returns
`refusalExit`: 1 for it, 2 for a malformed peer or one naming an account. Peer forms come first:
each entry point checks them against `agents.Names()` before any account (a fork loop in
`parseForkCreate`, before the fork is created), and `resolvePeerTargets` checks every form before
any sign-in, so a malformed peer exits 2 wherever it sits. Filtered editor sessions
(acp_network.go) and the ACP control only use the error as a signal and fall back, so they print
nothing from it. `coop usage provider@account` uses its own listing ([[usage-history-cache]]).
`coop credentials` lists what it could read and ends that agent's block with
`⚠ Could not list every <Agent> account` and the first cause; such a block offers no sign-in when
empty and never calls the default missing. Completion and hints ignore listing errors, and rotation
ignores them while its ladder still has an account to run.

The host privacy boundary is deliberately above provider-owned state. Configuration load and box
home preparation require the shared credential root, provider root, `profiles/`, and selected
profile to be real `0700` directories before reading or mounting them. Shared `env`, `defaults`,
and the default MCP file are `0600`. Coop does not recursively chmod provider transcripts or state;
the private ancestors protect those descendants without taking ownership of their formats.

## Changelog
- 2026-10-17 - a missing login now exits 1 from `coop acp --peer`'s lead expansion and a fork loop's
  `--peer` check too (-1, like the loop), and `needsAccountErr` is a `ui.CommandFailed`, so every
  peer check returns `refusalExit` (1 for no login, 2 for a malformed peer), with every peer's form
  checked before any account; added commands.go and loop_cmd.go to sources; verified against
  internal/cli/acp_cmd.go, fork_cmd.go, commands.go, loop_cmd.go, target.go and cli.go
- 2026-10-17 - the loop's review stages now return -1 like the work rotation (`rotationRefusal` is
  gone); recorded `runForkLoop`'s -1, the late re-login trade-off and that rotation ignores a failed
  listing only while its ladder can run; added acp_network.go, fork_cmd.go and loop.go to sources;
  verified against internal/cli/rotation.go, fork_cmd.go, acp_network.go, internal/loop/loop.go,
  review.go and internal/acpctl/control.go
- 2026-10-16 - ladder expansion now names a failed listing instead of advising `coop login` when
  nothing in the ladder can run, and the loop, its review stages, `coop acp --peer` and a fork
  loop's peer check exit 1 for it; verified against internal/cli/rotation.go, acp_cmd.go,
  acp_network.go, fork_cmd.go and internal/loop/loop.go and review.go
- 2026-10-16 - added account checks: what goes through `requireProfile` and what does not, the
  failed-listing refusal (`accountsUnlistedErr`, exit 1 through `refusalExit`) and the
  `coop credentials` warning; verified against internal/cli/profiles.go, cli.go, rotation.go,
  acp_cmd.go and acp_network.go
- 2026-10-16 - corrected drift: setting a default and removing an account check through
  `requireProfile`, which uses `EffectiveProfiles` (physical `Config.Profiles` when this card was
  written); verified against internal/cli/profiles.go
- 2026-09-15 - added adapter-owned API-key broker contracts and native reusable-key detection;
  verified every recognized env key is either brokered in its qualified direct filtered shape or
  refused before runtime, while OAuth/access-token projection remains separate
- 2026-09-13 - corrected the native Gemini API-key assumption after fresh-box reproduction and
  documented exact-account filtered admission for Coop-owned/API-key authority
- 2026-09-13 - allowed adapters to declare a native marker alongside selected env authority;
  verified Gemini native API-key presence, default env precedence, named-account isolation,
  Vertex denial, credential listing, and host-bound live preflight
- 2026-09-03 - documented and re-verified the owner-only host ancestor boundary and the deliberate
  decision not to rewrite provider-owned descendants
- 2026-08-25 - removed Fleet from the current consumer inventory; direct fork launches reuse the
  same target and profile resolution rather than owning a separate expansion path
- 2026-08-17 - added the shared runnable-credential refinement used by listing, rotation, and the
  ACP selector; swept ACP, CLI, proxy, and provider-process fixtures and replaced marker-only
  placeholders with adapter-valid records without changing broad presence callers
- 2026-07-16 - added adapter-owned stored-marker readiness without changing env-only or opaque presence semantics
- 2026-07-15 - aligned Gemini marker presence with the selected auth authority in box and live probes
- 2026-07-15 - created after unifying alternate-key, env-only, listing, rotation, fleet, and ACP credential discovery
