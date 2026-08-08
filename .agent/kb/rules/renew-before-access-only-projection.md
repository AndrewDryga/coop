---
name: renew-before-access-only-projection
description: refreshable credentials renew in trusted host storage before an access-only box projection
scope: security
sources: [internal/agent/agent.go, internal/agent/codex.go, internal/agent/claude.go, internal/cli/session_acp.go]
check: "go test ./internal/agent -run 'TestCodexCredentialRenewal|TestClaudeCredentialRenewal'"
updated: 2026-08-08
---

# Renew a refreshable credential before projecting it into a box

When an access token cannot outlive a turn, renew it while the complete credential remains in
trusted host storage. Serialize renewal, persist rotated tokens atomically, and create the
access-only sandbox projection only after renewal succeeds. Never copy refresh authority into the
box, and never report a refreshable login as ready while leaving the turn unable to use it.

**Why:** Responder surfaced `credential is not portable through the turn deadline` even though the
managed Codex profile retained valid refresh authority. Readiness and execution had implemented
different halves of the credential lifecycle.

**How to apply:** Keep preparation adapter-owned through `LiveCredentialSpec.Prepare`. The session
runner invokes it against the trusted source profile immediately before artifact projection. A
revoked refresh token becomes one authentication failure; it is not retried as a process crash.
Every adapter whose credential is refreshable owes a `Prepare`; declaring only `Portability` is
the defect, not a lighter variant of it.

## Changelog
- 2026-08-08 — wired the Claude adapter to the boundary. It had shipped with `Portability` and no `Prepare`, so an ~8h OAuth token expired into a hard turn failure while the source profile still held valid refresh authority; two live Responder deployments were down on Claude rungs. Endpoint, client id, and grant shape were read out of the shipped Claude Code binary rather than recalled — the remembered endpoint was wrong
- 2026-08-06 — created after sweeping the Codex readiness, renewal, projection, and ACP admission paths; focused tests cover rotation, eight concurrent callers, failure preservation, symlink rejection, and access-only child state
