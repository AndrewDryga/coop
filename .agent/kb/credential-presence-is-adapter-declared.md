---
name: credential-presence-is-adapter-declared
description: adapters own credential presence, selected env authority, and inspectable stored readiness
subsystem: credentials
sources: [internal/agent/agent.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/acpctl/control.go, internal/box/auth.go, internal/box/profiles.go, internal/cli/rotation.go, internal/cli/profiles.go, internal/testutil/liveprovider/credentials.go]
updated: 2026-09-13
---

Adapters own four credential facts: `AuthMarker` names their login file and canonical primary env
key; `CredentialEnvKeys` is the complete set of token keys they accept;
`ActiveCredentialEnvKeys` selects the env family authoritative for one profile; and
`StoredCredentialStatus` validates a native marker when its shape is safely inspectable. Presence
callers never reconstruct provider-specific precedence or OAuth schemas. `ProfileAuthed` and
`AuthedAgents` are the public profile and provider views over the shared presence predicate.

The env parser follows the runtime contract: non-empty `KEY=value` is present, and a bare `KEY`
imports the ambient value only when that variable exists. A present bare import or assignment wins;
an unset bare import is omitted and therefore does not clear an earlier assignment. Credential keys
must have one adapter owner because out-of-scope stripping is provider-based. `EffectiveProfiles`
adds an authenticated env-only default even without a profile directory, so `coop credentials`,
loop rotation, peer discovery, ACP defaults, and scoped mounts cannot disagree about
the same login. The env key is provider-wide, so it authenticates exactly the configured default;
other named profiles require their own marker file. Read, completion, and runnable-target paths use
`EffectiveProfiles`; default-setting and removal keep using physical `Config.Profiles` entries. A
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

The host privacy boundary is deliberately above provider-owned state. Configuration load and box
home preparation require the shared credential root, provider root, `profiles/`, and selected
profile to be real `0700` directories before reading or mounting them. Shared `env`, `defaults`,
and the default MCP file are `0600`. Coop does not recursively chmod provider transcripts or state;
the private ancestors protect those descendants without taking ownership of their formats.

## Changelog
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
