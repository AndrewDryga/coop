---
name: default-account-resolution
description: the default account is the marked one or the literal "default"; a first sign-in becomes it even when named, and a missing default beside signed-in accounts is refused with those accounts, never another login
subsystem: credentials
sources: [internal/config/config.go, internal/box/profiles.go, internal/box/network_bundles.go, internal/cli/commands.go, internal/cli/profiles.go]
updated: 2026-11-06
---
`Config.DefaultProfileOf` returns the account the `defaults` file marks for an agent, else the
literal `default`. A run with no `@account` resolves to it (`ActiveProfile`), and so does a bare
`coop login <agent>`. Nothing else names a default, so before 2026-11-06 a first
`coop login gemini@work` left the agent pointing at an account named `default` that did not exist.

`finishLogin` (both sign-in paths in `internal/cli/commands.go`) now marks a confirmed sign-in as the
default when it is the agent's only account in `box.EffectiveProfiles`. An env-file key counts as
an account there (the synthetic default), and revoked tombstones do not, so a named sign-in beside
an env key keeps `default`, and one after a revoked default takes over.

A default can still be missing: a defaults file naming an account deleted by hand, or a setup from
before that change. `box.MissingDefaultAccount` refuses that state when another account is signed
in, naming the signed-in accounts with `coop credentials <agent> <account> default` and
`coop <agent>@<account>`. It is checked in two places on purpose. `launchAgent` and `launchPreset`
check before `nudgeIfUnauthed`, in every network mode, so the TTY heads-up never prints a contrary
`coop login` above it. `NetworkTargetBundle` checks before its sign-in refusal, which covers loop,
fork, editor and preset-role admission. The target's account is never empty by then:
`networkTargetsForProvider` fills it with `ActiveProfile`, so only `account == DefaultProfileOf`
tells the default apart. With nothing else signed in the plain sign-in refusal still applies.

## Changelog
- 2026-11-06 — created with the first-account default and the missing-default refusal; verified
  against `finishLogin`, `MissingDefaultAccount`, `NetworkTargetBundle` and `accountBlock`.
