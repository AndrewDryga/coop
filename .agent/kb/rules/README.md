# .agent/kb/rules — the taste knowledge base

NORMATIVE constraints a change must obey: "do X, not Y." The normative floor of the one knowledge
tree: a card in `kb/` proper is DESCRIPTIVE ("here's how X actually works, and the trap"), a rule
here is a verdict you can fail a review against. A rule may link up to a card for background.

Every rule here started as a correction from the human. That's the intake: a correction becomes a
rule the same day, and the rule outlives the conversation that produced it.

## Reading protocol
Read this INDEX at boot; open a rule ONLY when your change touches its scope. Never bulk-load the
whole directory into a prompt — the index is the routing table, rules are pulled on demand (like
skills and kb cards). The exception is a deliberate full audit (`/review-board`), which reads every
rule against a diff on purpose.

## The check field — a rule wants to stop being prose
`check:` names the command that FAILS when the rule is violated, or `none` when only review catches
it. This is the graduation ladder made visible: a rule with `check: none` is one nobody has
mechanized yet, and

    grep -L 'check: none' .agent/kb/rules/*.md    # rules that gate themselves
    grep -l 'check: none' .agent/kb/rules/*.md    # rules still riding on review

is the honest scoreboard. When a rule becomes mechanically checkable, write the test or the make
target, put its command in `check:`, and say so in the changelog. Never name a command here that
doesn't exist or doesn't actually fail on a violation — a `check:` you can't run is worse than
`none`, because it claims a gate that isn't there.

`make rules-check` holds you to that, so the field takes one of three shapes: `none`, `make
<target>`, or `go test <pkg> -run <Test>` — the last also as a quoted alternation,
`go test ./internal/agent -run 'TestCodexCredentialRenewal|TestClaudeCredentialRenewal'`, when two
test families gate one rule. Quote the alternation (the shell would eat a bare bar) and expect every
alternative to be checked against real test functions.

## Validate a rule when you write it
A new rule is a claim about the whole tree, not just the diff that provoked it. Before committing it:

1. **Sweep for existing violations** — run its `check:`, or grep/read the surfaces in `sources:`.
2. **Record what you found** in the changelog: `swept N files, 0 violations` or `3 pre-existing
   violations → fixed here / queued as <task-id>`. A rule that has never been run against the tree
   is a hypothesis.
3. **A rule that fires everywhere or nowhere is wrong.** Hundreds of hits means it's describing a
   convention the codebase doesn't hold — narrow it or drop it. Zero hits, ever, on a rule that
   isn't preventive means it may be describing a problem that no longer exists.

Fixing the violations is usually a SEPARATE task ([[small-work-to-the-queue]]) — the rule commit
carries the rule, not a tree-wide refactor.

## You maintain this KB — directly
No inbox, no human gate. The discipline that replaces it is the metadata: every rule states when it
was last `updated`, what `scope` it governs, the `sources` it constrains, and whether a `check`
gates it — so staleness is obvious at a glance. When you pass through a scope, check its rules
against their `sources`; if one has drifted, re-verify and bump it (with a changelog line) or DELETE
it. A rule that contradicts the code is worse than no rule: it will be obeyed.

## Rule format
One rule per file: frontmatter, the rule, why it exists (cite the correction — that's the evidence),
how to apply it, and a changelog.

```
---
name: <kebab-case-slug>              # = the filename
description: <one line — judged for relevance straight from this index>
scope: <cli-grammar | cli-output | docs | box | loop | scaffold | security | architecture | agent-workflow>
sources: [internal/cli/help.go, …]   # the code/prose this governs — check drift against it
check: make align                    # command that fails on a violation, or `none`
updated: <YYYY-MM-DD>                # last edit
---

# <the rule as an imperative sentence>

<the constraint, then:>

**Why:** <the correction or incident that produced it — quote the human where you can>

**How to apply:** <the specific moves; link siblings with [[name]]>

## Changelog
- <YYYY-MM-DD> — created / what changed (and what you swept it against)
```

## Index

**CLI grammar** — the words the CLI accepts
- [list-verb-ls](list-verb-ls.md) — listing subcommands are `ls`, the only spelling (no `list` alias in v3)
- [destructive-verb-rm](destructive-verb-rm.md) — destructive subcommands are `rm`, the only spelling (no `remove` alias in v3)
- [credentials-not-profiles](credentials-not-profiles.md) — a stored account is publicly a "credential"; "profile" is retired, not aliased
- [model-is-the-rotation-axis](model-is-the-rotation-axis.md) — rotation walks an `agent:` ladder of targets; accounts are a suffix on the model, never their own axis
- [usage-placeholder-style](usage-placeholder-style.md) — one frozen `<angle>` lexicon for every usage string and error hint
- [bare-subcommand-shows-help](bare-subcommand-shows-help.md) — a bare group prints help or its default view, never an empty-token error
- [bare-flag-routes-to-default-view](bare-flag-routes-to-default-view.md) — a leading flag where a subcommand goes routes to the group's default listing

**CLI output** — what the terminal shows
- [help-output-style](help-output-style.md) — UPPERCASE headers, one command per line, no `·`, command cell ≤ 32 runes
- [command-output-tiers](command-output-tiers.md) — dim progress log, one `coop:` anchor, a bright next-steps block; standalone results use ✓/⚠/✗
- [no-color-in-width-fields](no-color-in-width-fields.md) — pad plain text to the column width, then style; never style inside `%-16s`
- [entity-blocks-with-labeled-fields](entity-blocks-with-labeled-fields.md) — multi-fact listings get one labeled block per entity, not one dense row
- [tag-exceptions-not-every-row](tag-exceptions-not-every-row.md) — tag only the exceptional row; explain the scheme once in a dim caption
- [list-output-echoes-source](list-output-echoes-source.md) — list output echoes the on-disk form, and grouped sections breathe
- [spinner-frames-animate-one-object](spinner-frames-animate-one-object.md) — spinner frames are successive states of one recognizable object
- [nonzero-progress-segments-stay-visible](nonzero-progress-segments-stay-visible.md) — a positive live or blocked count always gets at least one bar cell

**Docs**
- [align-trailing-comments](align-trailing-comments.md) — trailing `#` comments in an example line up in one column
- [docs-bold-sparingly](docs-bold-sparingly.md) — bold marks structure, never mid-sentence emphasis, never inline code

**The box**
- [box-logins-device-code](box-logins-device-code.md) — boxed agent logins use device-code/paste flows; browser OAuth hangs in a container
- [box-toolchain-on-login-path](box-toolchain-on-login-path.md) — a box toolchain goes on the login PATH too, not just `ENV PATH`
- [isolate-state-dont-serialize](isolate-state-dont-serialize.md) — when shared state breaks concurrency, isolate the state; never lock the users of it

**The loop**
- [loop-failover-profiles](loop-failover-profiles.md) — in the loop, failover swaps the active credential and never a session; the session API is the one surface that rotates the session itself
- [provider-reset-timezones-preserve-iana](provider-reset-timezones-preserve-iana.md) — preserve exact provider reset zones; parse safe IANA names and reject ambiguous abbreviations

**Scaffolding**
- [scaffold-fits-the-repo](scaffold-fits-the-repo.md) — `coop init` generates for the detected stack and stays neutral when it detects nothing
- [scaffold-config-reads-well](scaffold-config-reads-well.md) — scaffolded config leads every field with its comment and works as-is

**Security**
- [destructive-confirm-gate](destructive-confirm-gate.md) — every unrecoverable delete routes through the one shared `ui.DestroyGate`
- [renew-before-access-only-projection](renew-before-access-only-projection.md) — refreshable credentials renew in trusted host storage before an access-only box projection
- [secret-scan-literals-not-refs](secret-scan-literals-not-refs.md) — the scanner flags literal credentials and never references to them; precision is the product

**Architecture**
- [agents-are-one-file](agents-are-one-file.md) — a coding agent is one self-registering file in `internal/agent`, never a switch elsewhere
- [internal-import-dag](internal-import-dag.md) — a new internal import edge is an architecture decision — the allowlist test and this card move in the same commit
- [transport-bounds-do-not-abort-valid-work](transport-bounds-do-not-abort-valid-work.md) — bound retained state and single payloads, never the cumulative volume of valid work
- [repository-refresh-bounds-stalls-not-catch-up](repository-refresh-bounds-stalls-not-catch-up.md) — bound a stalled repository refresh without treating a normal catch-up fetch like one remote lookup
- [hermetic-git-tests](hermetic-git-tests.md) — a test that runs git pins `GIT_CONFIG_GLOBAL` *and* `GIT_CONFIG_SYSTEM`; identity envs alone still let the host's config in

**Agent workflow** — how an agent works here, not what it ships
- [fix-the-bug-not-the-feature](fix-the-bug-not-the-feature.md) — root-cause the misbehavior; deleting the feature is never the fix without the human
- [gate-is-one-recipe](gate-is-one-recipe.md) — a new check goes in `make check`; CI installs the tools and calls that target, never its own step list
- [small-work-to-the-queue](small-work-to-the-queue.md) — ready work goes to `00_todo/`; only the genuinely large or unscoped goes to the backlog
- [static-bounded-supervision](static-bounded-supervision.md) — supervised long commands keep output static and only a bounded excerpt in context
- [agent-instructions-use-in-box-capabilities](agent-instructions-use-in-box-capabilities.md) — agent-facing instructions name in-box capabilities, never host-side `coop fork`/`fleet`
- [shell-guards-fail-closed](shell-guards-fail-closed.md) — a shell guard checks every command's status and fails CLOSED on ambiguity; a script that fails open says so in its header
