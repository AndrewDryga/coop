# coop init scaffolds what the repo uses — it never imposes a stack

`coop init` is dogfooded from coop's own Go repo, which is exactly why it once shipped a
`gofmt` pre-commit gate into *every* repo — including a Terraform or Elixir one, where the
Go check is dead weight and the language actually in use gets no gate at all. Anything
`coop init` writes must fit the target repo, not coop's.

**Detect, then generate.** The commit gates (`.githooks/pre-commit` and
`.claude/hooks/commit-gate.sh`) are generated per stack — detected from marker files
(go.mod, `*.tf`, mix.exs, Cargo.toml) and `.tool-versions` — into `command -v`-guarded
format checks (gofmt / terraform fmt / mix format / cargo fmt). The guard means a check
runs in the box (toolchain provisioned) and silently skips on a host that lacks the tool.

**Preserve Git hook composition.** A repo-local `core.hooksPath` overrides the box's global path,
so the tracked hook directory also carries `.githooks/prepare-commit-msg`: a no-op-on-host shim that
chains `$HOME/.config/coop/git-hooks/prepare-commit-msg` inside a box. Never overwrite an existing
hook or a custom hooksPath. When a repo uses another hooks directory, init must tell the user to
copy or chain both tracked hooks there. When the active directory already owns a prepare hook, tell
the user to add the Coop-hook call to it; forcing the box path would silently discard repo hooks.

**When unsure, don't pollute — ask.** If nothing is detected, the gate is left **neutral**
(documented but inert: zero imposed checks) rather than guessing. At a terminal `coop init`
*asks* which gate to add; piped/CI (`!ui.IsTerminal(os.Stdin)`) it stays neutral and never
blocks. Guessing wrong is worse than doing nothing.

**How to apply:**
- New scaffolded artifact that's language-specific (a gate, a CI step, a Makefile target) →
  gate it on `scaffold.DetectStacks` (marker files + `.tool-versions`), don't hardcode one
  language. Add the language to `GateLangs` + `gateSnippets`, keep the snippet list-based
  (`gofmt -l`, `terraform fmt -check -list`) so a tool error fails *open* — only a real
  diff blocks the commit.
- Keep the scaffold pure: detection lives in `scaffold`, any interactive prompt lives in
  the CLI (`cmdInit`) so `scaffold.Init` never reads stdin (a prompt there would hang
  `go test`, whose stdin is often a tty).
- A no-clobber write (`writeContentIfAbsent`) so re-running `coop init` never overwrites a
  gate the user has since customized.
