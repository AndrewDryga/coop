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
separate credential list, never a bespoke model key. Resolution for a run, coarse to fine: the
explicit command-line target > the active ladder rung (loop.yaml step or preset lead) > the
agent-wide default (`COOP_<AGENT>_MODEL`) > the agent CLI's own default. Fan-out order for a
bare model is marked-default account first, then the rest alphabetically (`accountsFor`). See
[[loop-failover-profiles]] for how rotation swaps the active account each iteration, and
[[credentials-not-profiles]] for the user-facing naming.
