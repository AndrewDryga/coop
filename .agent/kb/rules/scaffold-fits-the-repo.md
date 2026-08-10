---
name: scaffold-fits-the-repo
description: "`coop init` generates for the detected stack and stays neutral when it detects nothing"
scope: scaffold
sources: [internal/scaffold/scaffold.go, internal/scaffold/gates.go]
check: "none"
updated: 2026-08-09
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
runs in the box (toolchain provisioned) and silently skips on a host that lacks the tool.

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
  stable failure text, `rustfmt --check -l`) so a tool error fails *open* — only a real diff
  blocks the commit. If a tool's check depends on project config it won't auto-discover outside
  its own wrapper (e.g. bare `rustfmt` doesn't read Cargo.toml's `edition` the way `cargo fmt`
  does), read that config directly rather than silently mis-checking.
- Keep the scaffold pure: detection lives in `scaffold`, any interactive prompt lives in
  the CLI (`cmdInit`) so `scaffold.Init` never reads stdin (a prompt there would hang
  `go test`, whose stdin is often a tty).
- A no-clobber write (`writeContentIfAbsent`) so re-running `coop init` never overwrites a
  gate the user has since customized.

## Changelog
- 2026-06-19 — created
- 2026-07-16 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: read internal/scaffold/gates.go and scaffold.go in
  full. Confirmed clean: `DetectStacks`/`knownStacks` marker files (go.mod/mix.exs/Cargo.toml) +
  `.tool-versions` + `*.tf` glob all current; `writeContentIfAbsent` (scaffold.go:249) guards
  every gate write (pre-commit :355, prepare-commit-msg chain :378, Claude gate :388); the
  interactive prompt lives in `cmdInit` gated on `ui.IsTerminal(os.Stdin)` (commands.go:1459-1465)
  and `internal/scaffold/*.go` never touches stdin. 1 violation found: `gateSnippets`' own comment
  (gates.go:85-86) says "Go and Terraform are list-based so a tool error fails open" — true for
  those two (`gofmt -l`/`terraform fmt -list` build a `bad` list; a tool error yields an empty
  list, not a block) — but elixir (:107-115) and rust (:116-123) gate directly on
  `mix format --check-formatted`/`cargo fmt --check`'s exit code, so an unrelated tool failure
  (broken toolchain, missing deps) blocks the commit exactly like a real formatting diff would,
  contradicting the rule's "only a real diff blocks the commit." Queued for the lead, not fixed
  here.
- 2026-08-09 — fixed the violation above. Elixir (gates.go:108-122) now loops
  `mix format --check-formatted` one staged file at a time and greps its stable "failed due to
  --check-formatted" text (unchanged across every installed Elixir from 1.14 through 1.20 —
  `Mix.Tasks.Format.check!/2` raises that exact message for a real diff and a different one,
  "mix format failed for file: …", for a crash) to tell the two apart; mix has no flag to list
  many files at once. Rust (gates.go:123-141) dropped whole-crate `cargo fmt --check` (which
  can't be scoped to just the staged files — it only ever formats what the crate's module graph
  discovers) for `rustfmt --check -l` per staged file, rustfmt's own `-l`/`--files-with-diff`
  being the direct gofmt-`-l` equivalent (verified against installed rustfmt 1.9); the guard
  moved from `command -v cargo` to `command -v rustfmt`, the binary actually invoked. Also found
  and fixed in passing: bare rustfmt defaults to the 2015 edition and hard-fails to parse
  `async`/`gen` code unless told otherwise (`cargo fmt` reads this from Cargo.toml automatically;
  bare `rustfmt` does not), which would have silently fail-opened on most real async Rust — now
  read straight from the crate's `Cargo.toml` `edition` key before the check. Both snippets now
  name the offending files in the block message and use a `<files>` fix-hint placeholder, matching
  go/terraform's existing style. Comment at gates.go:82-86 and this card's body updated to match.
  New behavioral coverage in `TestElixirRustGateListBased` (gates_test.go) runs the generated
  shell against stub mix/rustfmt binaries (no real toolchain in CI) proving: a real diff blocks
  and names only the dirty file; a crash on one file neither blocks nor swallows a real diff
  reported on another; the rust snippet's rustfmt invocation carries `--edition` read from
  Cargo.toml, or no `--edition` flag at all when there is none.
