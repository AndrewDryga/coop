---
name: menus-offer-only-accepted-tokens
description: "an interactive menu shows only the tokens the parser accepts, and names an unknown answer instead of dropping it"
scope: cli-grammar
sources: [internal/cli/commands.go, internal/cli/init_test.go]
check: "go test ./internal/cli -run TestPromptExactTokens"
updated: 2026-09-11
---

# An interactive menu offers exactly what it accepts — and rejects the rest out loud

A prompt that asks for one or more values prints ONE token per choice row: the exact string the
parser will accept, and nothing else beside it. An answer holding a token that isn't on the menu is
NAMED, with the accepted choices repeated, and the same question is asked again. Never accept the
recognized half of an answer and silently discard the rest.

**Why:** `coop init`'s language prompt read
`no stack detected — add a commit format gate? [go terraform elixir rust]` while its help said the
gate runs `gofmt`/`terraform fmt`/`mix format`/`rustfmt` — so people answered `gofmt`, and the
parser, which kept only tokens it recognized, scaffolded a NEUTRAL gate and said nothing. The user's
review of the onboarding flow called out both halves: a second bare token in a choice row "looks
selectable even though the parser accepts only its language", and silently ignoring an invalid
selection is an excluded near-miss, not a lenient default. A menu is a contract: everything on it
works, everything off it is refused where the user can see it.

**How to apply:**
- Render the menu from the SAME list the parser validates against
  (`scaffold.GateLangs`, `scaffold.ComposeServices`) — never a hand-written copy, and never a
  parenthetical naming the tool/flag/file a choice implies. Explain the outcome in the prose
  ABOVE the menu ("Coop will check their formatting before every commit"), where nothing can be
  mistaken for a choice.
- On an unknown token: `✗ Unknown <noun> “<token>”.` then `  Choose from: <the menu>`, then ask
  again. Keep the accepted tokens of that answer only if you re-ask for the whole answer — a
  partially applied answer is the failure this rule exists to stop.
- Blank means "none", and EOF means "none" too, so a closed stdin ends the loop instead of
  spinning on its own error. Route every question in one flow through ONE `bufio.Scanner`: a
  second scanner over the same stdin buffers past its own line and eats the next answer.
- A binary question keeps the shared `ui.Confirm`/`ui.ConfirmationResponse` parsing
  (`[Y/n]`/`[y/N]`) — this rule governs menus of values, not yes/no.
- The same discipline for a flag: `parseExplicitList` already names an unknown `--services` /
  `--agents` value and refuses. An interactive answer is held to the flag's standard, not below it.

See also [[scaffold-fits-the-repo]] (what init may generate at all) and
[[command-output-tiers]] (where the result of those answers is printed).

## Changelog
- 2026-09-11 — created, from the human-first `coop init` rework
  (task 2026-09-10-make-network-inspection-destination-first-and-ex). Swept every interactive
  prompt in the tree for the pattern: `grep -rn 'bufio.NewScanner(os.Stdin)|fmt.Scanln|ui.Confirm('`
  finds `internal/cli/net_approve.go`, `internal/cli/net_forget.go`, `internal/cli/commands.go`
  (`coop up`'s secret-file approval), `internal/forkctl/util.go` and `internal/ui/confirm.go` —
  all binary confirmations, out of scope and clean. The only value MENUS were `coop init`'s two
  (languages, services); both violated the rule and both are fixed in this commit, now sharing
  `promptExactTokens`. `TestPromptExactTokens` gates the menu contents, the named rejection and
  the re-prompt.
