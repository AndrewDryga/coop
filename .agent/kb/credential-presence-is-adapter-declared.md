---
name: credential-presence-is-adapter-declared
description: file, primary, alternate, and env-only credential truth converges in the box predicate
subsystem: credentials
sources: [internal/agent/agent.go, internal/box/auth.go, internal/box/profiles.go, internal/cli/rotation.go, internal/cli/profiles.go]
updated: 2026-07-15
---

Adapters own two credential facts: `AuthMarker` names their login file and canonical primary env
key; `CredentialEnvKeys` is the complete set of token keys they accept. Presence callers never
inspect the primary key themselves. `profileCredentialPresent` combines the adapter's marker file
with every declared effective env key; `ProfileAuthed` and `AuthedAgents` are the public profile and
provider views over that one predicate.

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
provider-wide token cannot shadow the account-specific marker mounted into the box. Marker presence
wins even in the default slot: only an env-backed default with no marker keeps the token.

Expiry, renewable-token, and mtime helpers remain marker-file refinements. They answer token health
or age after presence and should not grow independent env-key logic.

## Changelog
- 2026-07-15 - created after unifying alternate-key, env-only, listing, rotation, fleet, and ACP credential discovery
