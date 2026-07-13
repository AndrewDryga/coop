---
name: box-time-is-utc
description: boxes run UTC; the host TZ is forwarded so rate-limit reset prose parses back host-local
subsystem: box
sources: [internal/box/run.go, internal/cli/ratelimit.go]
updated: 2026-07-12
---
The box image's clock is UTC. coop forwards the HOST's timezone into every box as `-e TZ=...`
(`internal/box/run.go`, via `hostTimezone()`), so agents render clock times on your wall clock.

Why it matters: a rate-limit message often carries its reset time as PROSE ("try again at 4:28 PM"),
and coop parses that back with `time.ParseInLocation(layout, s, time.Local)`
(`internal/cli/ratelimit.go`) to schedule how long to wait. If a box rendered that time in UTC
instead of the host zone, the parsed wait would land HOURS off. So the box TZ and the host
`time.Local` must agree — if you touch either the TZ forwarding or the reset-time parser, keep them
on the same clock. See [[credentials-expired-is-a-false-alarm]] for the OAuth `expiresAt` clock that
rides the same wall time.

## Changelog
- 2026-07-12 — created: box clock is UTC, host TZ forwarded as `-e TZ`; must stay on the same clock as ratelimit.go's `time.Local` reset-time parser.
