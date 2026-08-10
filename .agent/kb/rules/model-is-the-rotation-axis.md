---
name: model-is-the-rotation-axis
description: "rotation walks an `agent:` ladder of targets; accounts are a suffix on the model, never their own axis"
scope: cli-grammar
sources: [internal/cli/target.go, internal/preset/preset.go, internal/sessionsvc/service.go]
check: "none"
updated: 2026-08-10
---

# Model is the one axis: rotation walks an `agent:` ladder of targets, not a pool

**The rule:** Every rotation/fallback surface names WHO runs the same way — a **target**,
`provider[:model][/effort][@account]` — and rotation is a ladder of targets. Accounts are a
suffix on the model, never their own axis.
- A preset's lead carries ONE `agent:` ladder (a target or a list of them, cross-provider
  allowed). A **bare `provider:model` fans out across every signed-in account** (marked-default
  first, then the rest); a pinned `provider:model@account` is one rung.
- Roles carry a single `agent:` target and NO credentials — a role runs on its agent's
  default account.
- A fleet fork takes a single `agent:` target (or a `preset:` for a full ladder) — never a
  plural list inline.
- `.agent/loop.yaml` steps (preflight/work/between/review) each take an `agent:` ladder whose
  rungs are targets **or preset names** (a preset rung brings its own lead ladder + roles).
- The launch names the target positionally: `coop claude:opus@work`. The old `--model` and
  `--credential` flags are RETIRED (tombstoned in target.go) — an account never carries a model,
  and a model is never a flag apart from its provider.
- There is NO persistent pool. `coop loop pool`/`pools.json` are gone; a stray `pools.json` is
  ignored silently. "Rotate all my accounts" is just what a bare-model rung already does.

**Why:** credentials and models were two competing axes for the same knob (which sub, which
model), which bred a credential-first `work@opus` shorthand, a `pools.json` registry, and
per-role credentials — three ways to say overlapping things. Making the **target** the single
spelling (model-first, accounts fan out under it) collapses all of that: the ladder is the
rotation, and a bare model is the old "rotate every account." The user drove this explicitly
("model first... drop pools, v3 is a clean sheet"); v4 finished it by folding provider, model,
effort, and account into the one target grammar.

**How to apply:** any new rotation/fallback surface takes targets in an `agent:` key — never a
separate credential list, never a bespoke model key. The ONE exception is the session API's
operator policy file, which spells the same thing `target:` because that key predates ladders and
is deployed; it takes a scalar or a list with identical parsing to a preset's `agent:`. Widening
the existing key was chosen over renaming it (every deployed policy file would break) and over
adding a `models:` list (a bespoke model key, and a rung is a target, not a model). Do not
"harmonize" it to `agent:`. Resolution for a run, coarse to fine: the
explicit command-line target > the active ladder rung (loop.yaml step or preset lead) > the
agent-wide default (`COOP_<AGENT>_MODEL`) > the agent CLI's own default. Fan-out order for a
bare model is marked-default account first, then the rest alphabetically (`accountsFor`). See
[[loop-failover-profiles]] for how rotation swaps the active account each iteration, and
[[credentials-not-profiles]] for the user-facing naming.

## Changelog
- 2026-08-09 — sources repointed: the sessions service moved out of `internal/cli/session_*.go` into `internal/sessionsvc/`; the facts here are unchanged (a move-only extraction).
- 2026-07-03 — created
- 2026-07-11 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-07 — session policies gained ladders; recorded why that surface keeps `target:` instead
  of `agent:`. Swept every rotation surface (preset lead, roles, loop.yaml steps, fleet forks,
  session policies): 0 violations — no surface carries a separate credential list or model key.
- 2026-08-09 — validate-on-write backfill: re-verified against internal/preset/preset.go (lead
  ladder via `leadLadder`; a role's `Agent` takes only `first.Provider` — one target, no ladder),
  internal/cli/fork_fleet.go (`fleetEntry.agent` is a single string; parseFleetYAML explicitly
  rejects an account ladder — "a fork takes one account"), internal/loopcfg/loopcfg.go (every
  step's `Agent` field is a `[]string` ladder), and internal/sessionsvc/service.go (`Target
  yaml.Node` — the one `target:`-spelled exception). Also re-ran the `--model`/`--credential`/
  `pools.json` greps from the 2026-08-07 sweep. 0 new violations; every claim still holds.
- 2026-08-10 — path-only, no claim change: the fleet surface this card re-verified moved to
  `internal/forkctl/fleet.go`, where the entry type and its parser are now exported
  (`FleetEntry.Agent`, `ParseFleetYAML`). The one-target-one-account rule and its explicit
  account-ladder rejection are byte-identical; only the names a grep would chase changed.
