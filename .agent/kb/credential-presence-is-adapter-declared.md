---
name: credential-presence-is-adapter-declared
description: file, primary, alternate, and env-only credential truth converges in the box predicate
subsystem: credentials
sources: [internal/agent/agent.go, internal/agent/gemini.go, internal/box/auth.go, internal/box/profiles.go, internal/cli/rotation.go, internal/cli/profiles.go, internal/testutil/liveprovider/credentials.go]
updated: 2026-07-15
---

Adapters own three credential facts: `AuthMarker` names their login file and canonical primary env
key; `CredentialEnvKeys` is the complete set of token keys they accept; and
`ActiveCredentialEnvKeys` selects the env family that is authoritative for one profile. Presence
callers never reconstruct provider-specific precedence. `ProfileAuthed` and `AuthedAgents` are the
public profile and provider views over the shared predicate.

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

Expiry, renewable-token, and mtime helpers remain marker-file refinements. They answer token health
or age after presence and should not grow independent env-key logic.

## Changelog
- 2026-07-15 - aligned Gemini marker presence with the selected auth authority in box and live probes
- 2026-07-15 - created after unifying alternate-key, env-only, listing, rotation, fleet, and ACP credential discovery
