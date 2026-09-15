---
name: run-teammates-share-credential-boundary
description: protect credentials at the run boundary; teammates share selected routes, while mutually untrusted agents use separate boxes
scope: security
sources: [AGENTS.md, internal/box/credential_broker.go, internal/box/network_bundles.go]
check: none
updated: 2026-09-15
---

# Protect credentials at the run boundary, not between teammates

Treat every model selected into one Coop run as a teammate inside one box-level trust boundary.
Use one run-scoped host broker with an exact provider/account route for each selected credential.
The real credential stays on the host; the box receives only temporary capabilities that select
those routes. Any teammate in that run may intentionally use the routes selected for the team.

Do not add one broker per model, process-level credential isolation, or separate boxes to the normal
preset/helper workflow. When agents must be mutually untrusted, make that an explicit separate-box
use case with its own product contract.

Route sharing does not make the broker a general forward proxy. A temporary capability remains
bound to its run, provider/account route, allowed upstream request shape, and cleanup lifecycle.

**Why:** The user chose the pragmatic long-term design: “All selected teammates share a box-level
trust boundary, while actual credentials remain outside it.” They explicitly rejected elaborate
security boundaries between cooperating models and reserved separate boxes for mutually untrusted
agents.

**How to apply:**

- Resolve all selected provider/account routes before launch and start one broker for the run.
- Keep real reusable keys out of environment variables, mounts, generated provider configuration,
  argv, logs, and transcripts; put only temporary route capabilities in the box.
- Make direct, loop, preset, helper, consult/delegate, ACP/editor, remote, and restricted workflows
  consume the same route plan instead of refusing merely because more than one teammate exists.
- Keep provider request shapes behind their adapters and bind each capability to an exact upstream;
  do not expose arbitrary destinations.
- Apply the same run-boundary pattern to remote tool credentials where practical, while keeping a
  tool route distinct from a provider/account route.

## Changelog

- 2026-09-15 — created from the explicit multi-account broker decision. Swept
  `internal/box/credential_broker.go` and `internal/box/network_bundles.go`: the current code selects
  one direct candidate and refuses presets, peers, ACP/remote, restricted and mixed-account runs.
  The violations are owned by queued task
  `2026-09-15-explain-protected-api-key-accounts-plainly-in-la`; remote MCP credential projection is
  separately owned by `2026-09-15-keep-remote-mcp-credentials-outside-agent-boxes`. No runtime code
  changed in this rule task.
