# Destructive subcommands use `rm` — the only spelling (no `remove` alias in v3)

Every destructive subcommand uses `rm` as its verb — in the dispatch, `usage:` string, help row, and
error suggestions: `coop fork rm`, `coop credentials rm`, `coop tasks rm`. It is the
ONLY accepted spelling: v3 keeps no `remove` alias.

**Why:** the user ran `coop profiles rm` and noticed `coop tasks` advertised `remove` instead —
"tasks use remove, here we use rm, we need consistency." `rm` is the unix/git/docker idiom. It was
first normalized with `remove` kept as a forgiving alias; then, for v3: "no need to keep backwards
compatible cli aliases, v3 can be clean from legacy." So `remove` was dropped — one spelling, no
dual-accepting compat. Mirrors [[list-verb-ls]] (ls is the only list verb).

**How to apply:**
- A new destructive subcommand → name it `rm` in the dispatch case, `usage:`, help row, and any "did
  you mean" list. Do NOT add a `remove` alias.
- Don't introduce a command whose primary destructive verb is `remove`/`delete`/`del`.
- Dispatch is a single `case "rm":` (no `, "remove"`). (In `coop fork`, a non-verb like `remove` is a
  fork *name*, not a verb — that's fine; fork names are open.)

See also [[destructive-confirm-gate]] (rm shares one confirmation gate), [[list-verb-ls]],
[[help-output-style]], and [[bare-subcommand-shows-help]].
