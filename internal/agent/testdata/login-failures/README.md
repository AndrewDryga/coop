What the pinned provider clients print when their login is refused, captured 2026-09-19 in the
`coop-clients` image with no real credentials (none, or a fake key; Grok against the replay issuer),
in the structured output mode the loop and the role wrappers run them in. `<provider>-<case>.stdout`
is the client's stdout, `.stderr` its stderr; a missing `.stderr` was empty. Claude reports it only on
stdout, in its JSON result.

`codex-workspace-routing.stdout` retains only the final stable error events captured from
Codex 0.159.2 on 2026-10-01 with a closed synthetic ChatGPT credential. Reconnect counters,
timestamps, request IDs and model-list/refresh chatter are omitted; no real credentials were used.
