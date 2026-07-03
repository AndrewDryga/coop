# The account concept is publicly named "credentials", never "profiles"

**The rule:** Every user-facing surface — command names, flags, help, errors, hints, docs —
says **credential(s)** for a stored account/login (a rate-limit slot). "Profile" is the
pre-v3 name and is RETIRED, not aliased: `coop profiles` and the `--profile` flag both
fail loudly with the rewrite to `credentials`/`--credential` (the v3 tombstone pattern —
no working aliases, ever). The only `--profile` that still works is an AGENT'S own flag
after a `--`. The RECIPE concept (lead + roles + models) is a **preset**, never a profile.

**Why:** "Profile" was carrying two unrelated meanings (an account, and a runtime
preference bundle), which is exactly what the credentials/presets split resolved. Any
new "profile" wording — or a surviving alias — re-muddies it; the user has directed
repeatedly that v3 carries NO legacy.

**Internal code is exempt:** on-disk layout (`<agent>/profiles/<name>/`), Go identifiers
(`ProfileAuthed`, `SetActiveProfile`, `DefaultProfileOf`), and file names stay — renaming
storage would force a data migration for zero user value. The boundary is what a USER
reads.

**Mechanical check:** grep new user-facing strings for `profile` before landing:
`grep -rn '"' internal/cli --include='*.go' | grep -i profile` should surface only
the alias-handling lines and internal identifiers.
