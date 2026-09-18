---
name: gemini-effort-thinking-settings
description: Gemini has no effort flag; coop's low/high become per-call system-settings files overriding the client's two thinking family bases — why bases not model names, why only low/high, and the traps
subsystem: agent
sources: [internal/agent/gemini.go, internal/agent/agent.go, internal/agent/target.go, internal/box/run.go, internal/acpctl/control.go, internal/cli/testdata/providerfixture/timeout.go]
updated: 2026-09-18
---

The pinned Gemini CLI (0.59.0) takes thinking only from its settings. Coop maps an effort to it
in `internal/agent/gemini.go`; everything below was captured at the REQUEST level against a
refusing local listener, in the `coop-clients` image (task artifact
`pinned-gemini-0.59.0-thinking.md` in `2026-09-15-map-coop-effort-to-gemini-thinking-controls`).

- **Override the family bases, never a model name.** Every chat model's config chain runs through
  `chat-base-3` (Gemini 3 and Gemma: `thinkingLevel`) or `chat-base-2.5` (`thinkingBudget`). The
  client remaps what it calls: `gemini-3-pro-preview` is sent as `gemini-3.1-pro-preview`, and on an
  API key `gemini-2.5-flash` is sent as `gemini-3.5-flash` (`setFlashModels`). A setting keyed on
  the typed name is silently a no-op; `customAliases` for the two bases reach whatever is called.
- **Only low and high.** The pinned SDK's `ThinkingLevel` has LOW and HIGH only, and any target can
  land on a Gemini 3 model (remap, auto routing, the 2.5 quota fallback chain), so `medium` and the
  rest are refused by `EffortSpec.Validate` rather than rounded. An effort on a model no base
  reaches is refused too — it would change nothing.
- **Per call, through `GEMINI_CLI_SYSTEM_SETTINGS_PATH`.** One file per level mounts at
  `~/.coop-gemini/thinking/<level>.json` (beside the profile, never inside it). The box's own Gemini
  gets the variable from `MCP` for `cfg.EffortFor`; consult/delegate arms re-choose by `$effort`
  with an `env` prefix, so a lead and a role can think at different levels in one box.
- **System settings merge objects, replace arrays.** `customAliases` deep-merges over the user's and
  the project's settings, so theirs survive; a `customOverrides`/`overrides` array written at the
  system layer would REPLACE theirs. Hence aliases. The one precedence gap: a `thinkingConfig` a
  user or project pins on a single model is more specific than a base and still wins.
- **Checked before launch.** `ParseTarget` validates what it can see; `box.checkEfforts` validates
  each scoped agent's and role rung's materialized model+effort (a `COOP_GEMINI_MODEL=x/low` default
  never went through the parser); the ACP controller refuses a `set_model` the session's effort
  cannot carry.
- **Test oracle.** The provider fixture reads a gemini call's effort from
  `GEMINI_CLI_SYSTEM_SETTINGS_PATH`, and its `timeout` stub accepts exactly that `env` prefix.
- **Pinned facts, floating boxes.** Base names, the model list and the LOW/HIGH enum are readings
  of 0.59.0, while non-filtered boxes still install `@latest` (0.60.0 was re-read: same shape).
  `TestGeminiThinkingIsQualifiedOnTheLockedClient` fails when the locked version moves; re-read the
  new bundle before moving its pin. The `gemini-3-flash` alias has no request-level proof: on an API
  key the client sends that name as `gemini-3.5-flash`, so only an account without 3.5 GA reaches it.
- **Probe trap.** `gemini --acp` from the host's Homebrew Node 26 sits silent on stdio; run ACP
  probes in the `coop-clients` image (Node 24), where it answers.

## Changelog
- 2026-09-18 — created with the Gemini effort mapping; the claims above were re-run against the
  pinned client in `coop-clients` (headless, per-call arm, resume, ACP set_model, user/project
  settings), except the `gemini-3-flash` alias noted above.
