---
name: acp-codex-turn-after-a-cancel
description: a Codex editor session's next turn after a cancelled turn sometimes skips its tools; the tools are in the request, refused settings play no part, and how to read Codex's own request trace to check
subsystem: acp
sources: [internal/acpctl/refused_settings_test.go, internal/acpproxy/cancel.go, internal/agent/codex.go]
updated: 2026-11-06
---
QA (2026-11-06) saw `coop acp codex:gpt-6-luna@personal` answer "I can't read files" with no tool call, after two
refused `session/set_config_option` calls and a turn cancelled 0.4 s after its prompt. Neither of those is the cause,
and no tool was lost.

Measured on v12.1.0-422 with real Codex (codex-acp 2.0.1, Codex 0.159.2), one tiny session each, a read prompt last:
- cancel, then read: 5 of 10 answered without a tool call. Two refusals, cancel, read: 8 of 24. A completed turn, then
  read: 0 of 8. The cancel is what matters; the refusals change nothing (the adapter throws `invalidParams` before it
  touches any state, and a refusal leaves Coop's target alone, which
  `TestRefusedSettingsAndACancelReachTheAdapterAsTheEditorSentThem` pins).
- Codex's trace of the model request for the tool-less turn is the request of a turn that read the file, with ids,
  times and the repository's commit hash aside: the same `additional_tools` item with `exec`, the same history with its
  `<turn_aborted>` note and the unanswered first message. It happened with a fresh websocket and with the reused one.
- The turn after the tool-less one worked every time (4 of 4).

So after an interrupted turn the model sometimes declines the tools it was given. Coop forwards the editor's lines
unchanged, the adapter keeps no state from the refusals, and Codex sends the tools. There is nothing for Coop to
restore, and a restart or a resend after a cancel would only be a guess. The QA driver's first prompt, `Reply with
exactly: OK`, may add to it, since the next prompt also says "reply with exactly".

How to look, with no Coop change: put `APP_SERVER_LOGS: /tmp/app-logs` and `CODEX_ROLLOUT_TRACE_ROOT: /tmp/codex-trace`
under `box.env` in a scratch repo's `.agent/project.yaml`, run the session, and copy both out with `docker exec` before
it closes. The log has every message between the adapter and Codex's app server; each trace bundle's `trace.jsonl`
points at one JSON file per model request (`inference_started`), and `RUST_LOG=codex_core::client=trace` adds why a
websocket request was not incremental. Read the bundle with `python3 -I`, since the box wrote it.

Trap: whether the cancel lands before the turn's request goes out decides if Codex reuses its prewarmed websocket for
the next turn (no `responses_websocket.connect` span in its log). It does not decide the outcome, so do not chase it.

## Changelog
- 2026-11-06 — created from 42 real sessions on `codex:gpt-6-luna@personal`, the adapter source at v2.0.1 and Codex's
  at rust-v0.159.2, and the replay test in `internal/acpctl/refused_settings_test.go`.
