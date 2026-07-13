---
name: credentials-expired-is-a-false-alarm
description: claude "token expired" in `coop credentials` still works in-box via the refresh token
verified: 2026-07-12
retire-when: box.ProfileRenewable is removed, or profileState stops treating expired-but-renewable as signed in
---
`coop credentials` can show a claude account as "token expired" (yellow) when its OAuth access token
is past `expiresAt`. That is NOT a dead login: if the credential carries a refresh token, the claude
CLI renews it on use and writes the fresh token back.

`box.ProfileRenewable` (`internal/box/profiles.go`) checks for that refresh token, and `profileState`
(`internal/cli/profiles.go`) treats expired-but-renewable as "signed in" — it surfaces "token
expired" only when there is NO refresh token. So before concluding an account is blocked, try a run:
an "expired" claude account usually answers fine in-box (seen live — an account shown "expired"
answered, then its `expiresAt` moved forward). The `rotated <age>` column in `coop credentials`
reads the mtime of that same token material (`box.ProfileTokenMtime`), which a refresh bumps. See
[[box-time-is-utc]] for the wall clock behind `expiresAt`.
