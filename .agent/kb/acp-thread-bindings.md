---
name: acp-thread-bindings
description: an editor thread's native session moves with every provider/account switch; the binding is persisted per thread so a fresh coop acp reopens the transcript the conversation actually continued on
subsystem: acp
sources: [internal/acpproxy/bindings.go, internal/acpproxy/proxy.go, internal/acpctl/bindings.go, internal/acpctl/control.go, internal/cli/acp_cmd.go]
updated: 2026-09-12
---

The editor knows a thread by one stable session id for its whole life. The box does not: every
provider or account switch re-creates the session on the new box under a fresh native id (a claude
transcript, a codex rollout, …), so a thread that switched N times is N+1 disjoint transcripts, and
only the newest one is where the conversation continues. In-process the proxy tracks that in
`proxy.sessions[editorID].adapterID` and remaps ids both ways (`internal/acpproxy/proxy.go`, `sess`).
The SIGHUP re-exec snapshot carries it across a binary swap — and nothing else did: a normal exit
forgot every binding, so the editor's next `session/load` carried the editor id to whatever provider
was active and replayed the stub written before the first switch (2026-09-12: a day of turns hidden
behind one "session limit" message).

`acpproxy.BindingStore` (`internal/acpproxy/bindings.go`) closes that gap. `bindSessionLocked` queues
a Save on every successful bind — session/new, session/load, a replay rebinding — and
`deleteSessionLocked` a Forget; `flushBindings` writes after `p.mu` is released. On an editor
`session/load|resume` the proxy resolves the id BEFORE the lock (`forwardClientControlled`). Bound to
the active provider: the load is forwarded under the native id and the reverse map is installed
provisionally, because the adapter streams the replayed history before the load result that commits
the binding; a failed load removes it. Bound to another provider: `Hooks.SelectProvider` selects it
like the Provider dropdown (`Control.SelectProvider`, `internal/acpctl/control.go`), the load is held
in `restartHeld`, and `releaseRestartHeld` re-forwards loads unconditionally once the new box is
published — the second pass resolves on the now-active provider. A store miss is byte-identical to
the pre-binding behaviour.

The store is `acpctl.ThreadBindings` (`internal/acpctl/bindings.go`): one 0600 JSON file per thread
under `<ConfigDir>/acp-threads/` — inside the private config tree no box mounts — named by a SHA-256
of the editor id (the adapter mints that id inside the box; it is never a host path), written with
`config.WriteFileAtomic`, validated on read (single-line tokens, size cap, editor id must match),
pruned after 90 days at open (past every adapter's own transcript retention). Wired once in
`cmdACPSupervise` (`internal/cli/acp_cmd.go`).

Not covered: stitching the earlier transcripts into the reopened thread. Coop only guarantees the
NEWEST native session comes back; the older fragments stay on disk under their own ids.

## Changelog
- 2026-09-12 — created with the cold-load resolution, the provider hold, and the file store; verified
  against `TestProxyColdLoad*`, `TestProxyBindingsFollowNewAndDelete`, `TestThreadBindings*`
