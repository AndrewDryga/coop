# A bare subcommand group shows help, never an "unknown command \"\"" error

`coop <group>` with no subcommand must print that group's help and exit 0 — not
`coop: unknown <group> command "" — use: …`. An empty token means "tell me the
options," which is exactly what help is for; `unknownErr` (with its did-you-mean) is
for a *mistyped* subcommand, not a *missing* one.

**Why:** `coop tasks` → `unknown tasks command "" — use: list, lint, add, split`
reads as an error for doing nothing wrong, and buries the options in a one-line scold.
Bare `coop` prints help; a bare group should match that.

**How to apply:**
- In every group dispatcher, branch on the empty subcommand BEFORE `unknownErr`:
  `case "": return groupHelp("<group>")` (helper in help.go). Keep `unknownErr` only
  for a non-empty, unrecognized token.
- A group that has a *useful default view* may show that instead of help — the
  invariant is "never the empty-token error," not "always help." Current sweep:
  `tasks`, `fleet` → help; `pool` → shows the pool; `profiles` → lists profiles;
  `fork` → `forkHelp`. None emits the empty-token error.
- Not easily lintable (it needs flow analysis of each dispatcher), so this stays a
  reviewed rule; check it whenever you add or touch a subcommand group.
