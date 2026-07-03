# The account concept is publicly named "credentials", never "profiles"

**The rule:** Every user-facing surface — command names, flags, help, errors, hints, docs —
says **credential(s)** for a stored account/login (a rate-limit slot). "Profile" is the
pre-v3 name: `coop profiles` tombstones to `coop credentials`, and `--profile` survives
only as an accepted alias of `--credential` (never shown in examples or hints). The
RECIPE concept (lead + roles + models) is a **preset**, never a profile.

**Why:** "Profile" was carrying two unrelated meanings (an account, and a runtime
preference bundle), which is exactly what the credentials/presets split resolved. Any
new "profile" wording re-muddies it.

**Internal code is exempt:** on-disk layout (`<agent>/profiles/<name>/`), Go identifiers
(`ProfileAuthed`, `SetActiveProfile`, `profilePool`), and file names stay — renaming
storage would force a data migration for zero user value. The boundary is what a USER
reads.

**Mechanical check:** grep new user-facing strings for `profile` before landing:
`grep -rn '"' internal/cli --include='*.go' | grep -i profile` should surface only
the alias-handling lines and internal identifiers.
