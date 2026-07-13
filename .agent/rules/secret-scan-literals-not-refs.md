# A secret scanner flags literal credentials, never references to them

The fuzzy half of `box.ScanSecrets` (the entropy heuristic on a value assigned to a
secret-named key) exists to catch a *real token pasted into a file*. It must not fire on
the far more common thing that looks superficially similar: **code and config that names
or references a secret**. The user's bar — "the scanning gives a ton of false positives on
innocent code, e.g. `databricks_api_key = var.blitz_databricks_api_key`" — is the bar.

Two independent gates, both required before the entropy check runs:

1. **The key must *end* in a credential word**, not merely contain one. A credential key is
   `<context>_<secret-word>` (`client_secret`, `auth_token`, `databricks_api_key`); a config
   key that happens to contain the word doesn't end in it (`authenticator`, `token_url`,
   `allocate_tokens`, `auth_provider`). Anchor the word at the end of the key —
   `(?:…|secret|token|credentials?)(?:[_-](?:b64|json|pem|…))?` followed by `\s*[:=]`.

2. **The value must look like a literal, not a reference.** A real token is random:
   base64/hex, no English words, no code punctuation. Anything structured is a reference and
   must be skipped — variable/config refs (`var.x`, `process.env.X`, dotted paths), `${…}` /
   `{{…}}` interpolations, function calls `f(…)`, brackets `[…]`, Rust generics `T<U>` /
   namespaces `a::b`, Elixir module attributes `@attr`, `SCREAMING_SNAKE` and `snake_case`
   bare identifiers, comment lines, a match drowning in a minified/generated line (>2 KB of
   line content *around* the assignment — key on the slack, not the line length, so a huge
   all-value line like a base64-encoded service-account blob still fires), URLs and
   filesystem paths, and placeholder/example/fixture vocabulary (`…EXAMPLE`, `your-…`,
   `changeme`, `"very-long-password-1234"`, `"fake-payment-method-token"`).

**Why:** the cost is asymmetric and the asymmetry is the whole point. A scanner that cries
wolf on every `api_key = var.x` gets ~1,900 hits on one app and is turned off — at which
point it catches *zero* real secrets. A scanner that surfaces 1–15 genuine literal shapes
per repo gets read. Precision is what makes it useful; recall on obfuscated edge cases
(a token a dev prefixed with "secret") is worth trading away. The discriminator is
reliable because the alphabets don't overlap: base64/hex tokens contain no `.`, `::`, `<`,
`@`, `{`, `/`, space, or dictionary word, so excluding values that *do* can't hide a
random token.

**How to apply:**
- New language/idiom throwing a false positive? Find the *structural* tell that no random
  token has (a punctuation char, a casing convention, a vocabulary word) and add it to
  `looksLikeCodeRef` / `placeholderRe` — never widen by raising the entropy threshold or
  narrowing key names ad hoc.
- The provider-pattern half (`secretPatterns`) is precise by construction and scans every
  line, comments included — but still gate its matches through `placeholderRe` so canonical
  example tokens (`AKIA…EXAMPLE`) don't fire.
- A credential *format* a structural guard suppresses gets its own provider pattern, never a
  guard exception: a JWT parses as a dotted code ref, so `eyJ….eyJ….sig` is matched precisely
  before the entropy path ever sees it.
- A shape that overlaps the credential alphabet is NOT a valid skip, however common the
  fixture: a canonical UUID is hex+dash and real credentials ARE UUIDs (Heroku API keys are
  lowercase v4), so a UUID value on a credential key keeps firing
  (`TestScanSecretsUUIDValueStillFlagged` pins this). If UUID fixture noise ever dominates,
  the fix is a non-secret tell (fixture vocabulary, known doc UUIDs) — not "UUID is safe".
- Every change ships with both a true-positive that must still flag (a random value on that
  key shape) and the false-positive it kills, in `secretscan_test.go`. Real reductions are
  measured against actual repos, not invented strings.
- Vendored/build output is excluded *before* scanning (git-tracked + untracked, gitignored
  paths dropped — `candidateFiles`), so the scanner never sees `node_modules/`, `dist/`,
  `_build/`. That enumeration is the single biggest noise win; keep it.
