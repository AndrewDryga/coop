# Destructive subcommands advertise `rm` (and accept `remove` as an alias)

Every destructive subcommand uses `rm` as its canonical verb and shows `rm` in its usage, help row,
and error suggestions — `coop fork rm`, `coop profiles rm`, `coop loop pool rm`, `coop tasks rm`. `remove`
is accepted everywhere as a forgiving alias, but is never the form advertised.

**Why:** the user ran `coop profiles rm` and noticed `coop tasks` advertised `remove` instead —
"tasks use remove, here we use rm, we need consistency." `rm` is the unix/git/docker idiom and was
already the verb for fork/profiles/pool; `coop tasks` was the lone outlier (it advertised `remove`
while quietly accepting both).

**How to apply:**
- A new destructive subcommand → name it `rm` in the dispatch case, the `usage:` string, the help
  row, and any "did you mean" suggestion list. Add `remove` to the accept-set (the `case`/check) so
  old muscle memory and scripts keep working — but do not show it anywhere.
- Don't introduce a command whose primary destructive verb is `remove`/`delete`/`del`.
- Keep the accept-set uniform: if one destructive command accepts `remove`, they all do.

See also [[destructive-confirm-gate]] (rm shares one confirmation gate), [[help-output-style]], and
[[bare-subcommand-shows-help]].
