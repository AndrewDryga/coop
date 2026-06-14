---
name: verify-api
description: Confirm a function, argument, option, type, callback, or CLI flag actually exists with the signature you think — before you call it. Use whenever you're about to use an API you're not certain of, or any time you'd otherwise be guessing. Stops hallucinated calls before they're written.
argument-hint: "<the function/option/flag you're unsure about>"
allowed-tools: Read, Grep, Glob, Bash, WebFetch
---

# Verify the API — don't invent it

A hallucinated function/arg/flag costs far more than the lookup. When in doubt,
**check before you write.** Stop at the first rung that gives a definitive answer;
local sources beat the web — they're exact for the versions this repo uses.

## The ladder (highest authority first)
1. **This repo — for our own code.** The source IS the spec. Grep for the
   definition, then read it and its signature/docstring.
   ```sh
   rg -n 'def my_function|fn myFunction|function myFunction' <dir>
   ```
2. **Dependency source (vendored / lockfile-pinned).** The exact version's real
   code — `node_modules/`, `deps/`, `vendor/`, the site-packages path — not a guess
   or a newer doc. Confirm the version from the lockfile first if it matters.
3. **Tooling help / REPL.** A CLI's real flags or a quick signature:
   ```sh
   <tool> --help        # real flags for this installed version
   <tool> help <cmd>
   ```
   Or the language REPL for a function's docs/signature.
4. **Official docs (web) — last resort.** Only when local source can't answer, and
   match the version you actually depend on. Beware blog posts and newer-version docs.

Report what you confirmed (the signature) and where you found it. If you can't
confirm it, say so — and pick a different API you *can* verify.
