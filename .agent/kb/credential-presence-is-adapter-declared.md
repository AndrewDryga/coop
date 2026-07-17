---
name: credential-presence-is-adapter-declared
description: adapters own credential presence, selected env authority, and inspectable stored readiness
subsystem: credentials
sources: [internal/agent/agent.go, internal/agent/claude.go, internal/agent/gemini.go, internal/agent/grok.go, internal/box/auth.go, internal/box/profiles.go, internal/cli/rotation.go, internal/cli/profiles.go, internal/testutil/liveprovider/credentials.go]
updated: 2026-07-16
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
loop/fleet expansion, peer/Fusion discovery, ACP defaults, and scoped mounts cannot disagree about
the same login. The env key is provider-wide, so it authenticates exactly the configured default;
other named profiles require their own marker file. Read, completion, and runnable-target paths use
`EffectiveProfiles`; default-setting and removal keep using physical `Config.Profiles` entries. A
run on a marker-backed profile strips that provider's env keys before passing the env file, so the
provider-wide token cannot shadow the account-specific marker mounted into the box. A marker owns
presence only when the adapter selects no env authority. This matters for Gemini: `gemini-api-key`
accepts only `GEMINI_API_KEY`, `vertex-ai` accepts only `GOOGLE_API_KEY`, and `oauth-personal`
requires the marker. A stale OAuth marker cannot make a mismatched API-key selection look signed in.
The live credential isolator uses the same rule before it decides whether a real prompt may run.

After presence succeeds, `coop credentials` asks the adapter for marker readiness only when that
exact profile has a marker. Claude and Grok distinguish usable or refreshable OAuth from malformed,
stripped, and nonrenewable records; Codex and Gemini return unknown and preserve presence-based
behavior. An env-only default therefore never becomes "re-login required" because a marker file is
absent. `ProfileTokenMtime` remains the separate marker-age refinement and should not grow provider
schema or env-key logic. Live projection is a stricter, separate contract: source refreshability may
make a stored login ready, but refresh authority is still omitted from the projected box credential.

## Changelog
- 2026-07-16 - added adapter-owned stored-marker readiness without changing env-only or opaque presence semantics
- 2026-07-15 - aligned Gemini marker presence with the selected auth authority in box and live probes
- 2026-07-15 - created after unifying alternate-key, env-only, listing, rotation, fleet, and ACP credential discovery
