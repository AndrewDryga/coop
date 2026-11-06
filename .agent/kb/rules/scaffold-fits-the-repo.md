---
name: scaffold-fits-the-repo
description: "`coop init` generates for the detected stack and stays neutral when it detects nothing"
scope: scaffold
sources: [internal/scaffold/scaffold.go, internal/scaffold/gates.go, internal/scaffold/projectfile.go]
check: "none"
updated: 2026-11-06
---

# coop init scaffolds what the repo uses — it never imposes a stack

`coop init` is dogfooded from coop's own Go repo, which is exactly why it once shipped a
`gofmt` pre-commit gate into *every* repo — including a Terraform or Elixir one, where the
Go check is dead weight and the language actually in use gets no gate at all. Anything
`coop init` writes must fit the target repo, not coop's.

**Detect, then generate.** The commit gates (`.githooks/pre-commit` and
`.claude/hooks/commit-gate.sh`) are generated per stack — detected from marker files
(go.mod, `*.tf`, mix.exs, Cargo.toml) and `.tool-versions` — into `command -v`-guarded
format checks (gofmt / terraform fmt / mix format / rustfmt). The guard means a check
runs where the toolchain is installed and, where it is not, skips with a warning naming the
missing tool, never in silence: Coop's default box carries none of these toolchains, so a
silent skip passed every commit in a box unchecked. Where the tool is there but fails, gofmt and
terraform fmt stop the commit on one line naming the tool and its first complaint: they read no
project settings, so a failure (a shim with no version set, a file they cannot parse) means
nothing was checked. A detected language the project's box lacks (no project Dockerfile, not
pinned in `.tool-versions`) gets init's exact next step: the `.tool-versions` pin and
`coop init --stack asdf` (`scaffold.Toolchains`).

**Preserve Git hook composition.** A repo-local `core.hooksPath` overrides the box's global path,
so the tracked hook directory also carries `.githooks/prepare-commit-msg`: a no-op-on-host shim that
chains `$HOME/.coop-git-hooks/prepare-commit-msg` inside a box. Never overwrite an existing
hook or a custom hooksPath. When a repo uses another hooks directory, init must tell the user to
copy or chain both tracked hooks there. When the active directory already owns a prepare hook, tell
the user to add the Coop-hook call to it; forcing the box path would silently discard repo hooks.

**Keep one established skills source.** Prefer an existing `.agent/skills`; otherwise adopt a real
`.claude/skills` before scaffolding `.agent/skills`. Point new agent links at that source, but do not
seed Coop templates into an adopted project-owned directory, and preserve any existing link that
already resolves to a directory. Box-time synthesis must use the same source priority so an agent
omitted during init still sees the repo's skills.

**When unsure, don't pollute — ask.** If nothing is detected, the gate is left **neutral**
(documented but inert: zero imposed checks) rather than guessing. At a terminal `coop init`
*asks* which gate to add; piped/CI (`!ui.IsTerminal(os.Stdin)`) it stays neutral and never
blocks. Guessing wrong is worse than doing nothing.

**How to apply:**
- New scaffolded artifact that's language-specific (a gate, a CI step, a Makefile target) →
  gate it on `scaffold.DetectStacks` (marker files + `.tool-versions`), don't hardcode one
  language. Add the language to `GateLangs` + `gateSnippets`, keep the snippet list-based
  (`gofmt -l`, `terraform fmt -check -list`, `mix format --check-formatted` matched against its
  stable failure text, `rustfmt --check -l`) so it names exactly the files to fix. A formatter
  that reads no project settings fails *closed*: gofmt or terraform fmt exiting non-zero without
  listing the file stops the commit on its first complaint
  (TestPreCommitHookStopsWhenTheFormatterFails). mix format and rustfmt also load the project's
  own setup (its deps and formatter config, rustfmt.toml, the edition read from Cargo.toml), so
  their other failures fail *open* — only a real diff blocks the commit. If a tool's check
  depends on project config it won't auto-discover outside its own wrapper (e.g. bare `rustfmt`
  doesn't read Cargo.toml's `edition` the way `cargo fmt` does), read that config directly
  rather than silently mis-checking.
- Keep the scaffold pure: detection lives in `scaffold`, any interactive prompt lives in
  the CLI (`cmdInit`) so `scaffold.Init` never reads stdin (a prompt there would hang
  `go test`, whose stdin is often a tty).
- A repository-anchored, exclusive no-clobber write (`writeContentIfAbsent`) so re-running
  `coop init` never overwrites a gate the user has since customized or follows a repository-owned
  link. The documented project-owned `prepare-commit-msg` composition path is the deliberate
  exception: preserve it and print chaining guidance.

## Changelog
- 2026-11-06 — gofmt and terraform fmt fail closed. A formatter on PATH that failed (the asdf shim
  with no version set exits 126, a file it cannot parse) passed the commit in silence, so
  unformatted or unparsable Go landed (overnight QA, finding F9). The hook now stops the commit with
  "pre-commit blocked — <tool> failed: <first complaint>", checked against the real asdf shims,
  gofmt 1.27.3 and terraform 1.17.1 (TestPreCommitHookStopsWhenTheFormatterFails runs the rendered
  hook through git commit). Swept the four snippets: rustfmt and mix format pass a failure in
  silence the same way, but stay fail-open under the 2026-08-09 decision below, since their
  failures are often the project's setup; whether they should fail closed too is the owner's call.
- 2026-11-03 — the guard no longer skips silently: a missing formatter prints "pre-commit: <tool>
  is not installed here, so the <Language> format check was skipped" and the commit proceeds
  (TestGateWarnsWhenTheFormatterIsMissing). A Go repo's default box had no gofmt, so its promised
  check never ran. init names the toolchain the box lacks with the pin and build commands, the Go
  release read from go.mod (TestToolchains, TestInitActions). Swept the four snippets: all four
  guarded silently; all four warn now.
- 2026-10-17 — the 2026-08-09 entries name `installGitHooks` and the `gateSnippets` entries instead
  of scaffold.go/gates.go/commands.go line numbers.
- 2026-10-16 — pass-through for the skills-link handle: a link that already resolves to a
  directory is still kept (`linkSkillsIfAbsent`'s keep check, `TestInitSkillsSource`); only where
  the replacement happens changed, now through one parent handle. Swept internal/scaffold: no
  by-path removal or link creation left.
- 2026-10-15 — pass-through for the concurrent-edit removal (audit decision 3.5): `RegisterSubprojects`
  no longer rechecks identity and content or retries before its rename, so the 2026-09-03 recheck
  below is gone; the edit now reads, stages and renames through one directory handle and refuses a
  leaf link. The exclusive no-clobber create this rule relies on is unchanged. Swept
  internal/scaffold: 0 recheck-before-replace callers left; writeNewRepoFile still compares identity
  before removing its partial file (a box can make that write fail by filling the disk).
- 2026-09-10 — pass-through for the explicit-filtered project file: `projectYAML` now writes an active
  `box.egress: filtered` (a network posture every stack shares — not a stack guess, so detection is
  untouched) with a leading comment, and the file spells no-network access `offline`. Re-init stays
  the `writeNewRepoFile` no-clobber path; `TestScaffoldedProjectParses` proves an edited file is kept.
- 2026-09-03 — replaced scaffold check-then-write calls with one `os.Root`-anchored exclusive
  creator. Existing regular files remain the supported re-init no-op; links and unsupported
  entries fail before any outside target can change. `RegisterSubprojects` now validates the exact
  opened bytes, stages a synced sibling file, rechecks identity and content, and retries a
  concurrent valid edit before atomic replacement. Kept the explicit project-owned prepare-hook
  symlink contract rather than weakening it under the general file rule.
- 2026-08-26 — removed the pre-v8 byte-signature and gitignore rewrite migrations from re-init.
  Fresh/current scaffold output and the `writeContentIfAbsent` non-clobber boundary are unchanged;
  direct pre-v8 upgrades are now an explicit v9 migration step instead of permanent init branches.
- 2026-06-19 — created
- 2026-07-16 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: read internal/scaffold/gates.go and scaffold.go in
  full. Confirmed clean: `DetectStacks`/`knownStacks` marker files (go.mod/mix.exs/Cargo.toml) +
  `.tool-versions` + `*.tf` glob all current; `writeContentIfAbsent` (scaffold.go) guards
  every gate write in `installGitHooks` (pre-commit, prepare-commit-msg chain, Claude gate); the
  interactive prompt lives in `cmdInit` gated on `ui.IsTerminal(os.Stdin)` (commands.go)
  and `internal/scaffold/*.go` never touches stdin. 1 violation found: `gateSnippets`' own comment
  (gates.go) says "Go and Terraform are list-based so a tool error fails open" — true for
  those two (`gofmt -l`/`terraform fmt -list` build a `bad` list; a tool error yields an empty
  list, not a block) — but its elixir and rust snippets gate directly on
  `mix format --check-formatted`/`cargo fmt --check`'s exit code, so an unrelated tool failure
  (broken toolchain, missing deps) blocks the commit exactly like a real formatting diff would,
  contradicting the rule's "only a real diff blocks the commit." Queued for the lead, not fixed
  here.
- 2026-08-09 — fixed the violation above. Elixir (`gateSnippets["elixir"]`) now loops
  `mix format --check-formatted` one staged file at a time and greps its stable "failed due to
  --check-formatted" text (unchanged across every installed Elixir from 1.14 through 1.20 —
  `Mix.Tasks.Format.check!/2` raises that exact message for a real diff and a different one,
  "mix format failed for file: …", for a crash) to tell the two apart; mix has no flag to list
  many files at once. Rust (`gateSnippets["rust"]`) dropped whole-crate `cargo fmt --check` (which
  can't be scoped to just the staged files — it only ever formats what the crate's module graph
  discovers) for `rustfmt --check -l` per staged file, rustfmt's own `-l`/`--files-with-diff`
  being the direct gofmt-`-l` equivalent (verified against installed rustfmt 1.9); the guard
  moved from `command -v cargo` to `command -v rustfmt`, the binary actually invoked. Also found
  and fixed in passing: bare rustfmt defaults to the 2015 edition and hard-fails to parse
  `async`/`gen` code unless told otherwise (`cargo fmt` reads this from Cargo.toml automatically;
  bare `rustfmt` does not), which would have silently fail-opened on most real async Rust — now
  read straight from the crate's `Cargo.toml` `edition` key before the check. Both snippets now
  name the offending files in the block message and use a `<files>` fix-hint placeholder, matching
  go/terraform's existing style. `gateSnippets`' comment and this card's body updated to match.
  New behavioral coverage in `TestElixirRustGateListBased` (gates_test.go) runs the generated
  shell against stub mix/rustfmt binaries (no real toolchain in CI) proving: a real diff blocks
  and names only the dirty file; a crash on one file neither blocks nor swallows a real diff
  reported on another; the rust snippet's rustfmt invocation carries `--edition` read from
  Cargo.toml, or no `--edition` flag at all when there is none.
