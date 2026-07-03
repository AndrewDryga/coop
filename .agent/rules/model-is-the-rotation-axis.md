# Model is the one axis: a loop rotates a model-first `models:` ladder, not a pool

**The rule:** The unattended loop's rotation is expressed model-first and nowhere else.
- A preset's lead carries ONE `models:` ladder (no lead `model:`, no lead `credentials:`).
  Each entry is `model` or `model@account`. A **bare model fans out across every signed-in
  account** (marked-default first, then the rest); a pinned `model@account` is one target.
- Roles carry a single `model:` and NO credentials — a role runs on its agent's default account.
- A fleet fork takes a single `model:` (may be `model@account`) + single `credential:`. A full
  ladder → point the fork at a `preset:`, never a plural list.
- Launch flags are single-value: `--model` and `--credential`. The shortcut is
  `--model opus@work` (model-first). The credential-first `--credential work@opus` `@`-form is
  RETIRED — an account never carries a model.
- There is NO persistent pool. `coop loop pool`/`pools.json` are gone; a stray `pools.json` is
  ignored silently. "Rotate all my accounts" is just what a bare ladder model already does.

**Why:** credentials and models were two competing axes for the same knob (which sub, which
model), which bred a credential-first `work@opus` shorthand, a `pools.json` registry, and
per-role credentials — three ways to say overlapping things. Making **model** the single axis
(accounts fan out under it) collapses all of that: the ladder is the rotation, and a bare model
is the old "rotate every account." The user drove this explicitly ("model first... drop pools,
v3 is a clean sheet").

**How to apply:** any new rotation/fallback surface names models, with accounts as an optional
`@account` suffix — never a separate credential list. Precedence when resolving a run's model:
`--model` / fleet `model:` > active ladder entry's model > `COOP_LOOP_MODEL` / preset >
the account's mark > `COOP_<AGENT>_MODEL` > agent CLI default. Fan-out order is
marked-default account first, then the rest alphabetically (`accountsFor`). See
[[loop-failover-profiles]] for how the rotation swaps the active account each iteration, and
[[credentials-not-profiles]] for the user-facing naming.
