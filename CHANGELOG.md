# Changelog

## 2.4.0

- **`coop clone` is now `coop fork` — a full local-PR lifecycle, not a one-shot
  handoff.** A fork is a throwaway local clone (own `origin`, nowhere to push, no
  gitignored secrets); treat it like a contractor's PR — open, review, merge, close:
  - `coop fork <name> [claude|codex|gemini]` — open or re-enter a fork and run the
    chosen agent. **Re-entering continues your last session by default** (the fork's
    history persists): claude `--continue`, gemini `--resume latest`, and codex by the
    session whose recorded `cwd` is that fork — each **scoped to this fork** so it never
    resumes an unrelated session. Falls back to fresh when none exists; `--new` forces a
    fresh session.
  - `coop fork ls` — list this repo's forks with branch, change size, last activity;
    `coop fork open <name>` prints its path.
  - `coop fork review <name>` — fetch the fork's branch into `review/<name>` and show
    the diff (no more hand-typed `git fetch … && git diff`).
  - `coop fork merge <name>` — land it by **rebasing** the fork onto your branch
    (linear history, no merge commit), then offer to close it; refuses if your tree
    is dirty.
  - `coop fork rm <name>` — discard a fork; refuses while its work is unmerged or
    dirty unless `--force`.
  Forks live in a sibling `../<repo>-forks/` (was `-agents/`). `coop clone` stays a
  back-compat alias, and `coop dispatch` now forks into the same place.
- **A fleet of forks, each on a different model, looping in the background.**
  `coop fork <name> <agent> --loop` runs the unattended queue loop in a fork with the
  chosen model — claude (`-p`), codex (`exec`), or gemini (`-p`) — seeding it from the
  `.agent/TASKS.<name>.md` slice. Add `-d` to detach it (session-leader background
  worker; logs captured to `../<repo>-forks/.coop/<name>.log`). New process commands:
  `coop fork logs [name] [-f]` (no name = every fork at once, prefixed),
  `coop fork stop <name>`, and a running/idle column in `coop fork ls`. `coop dispatch`
  now takes an optional agent too.
- **Forks land by rebasing, and revalidate before they land.** `coop fork merge`
  rebases the fork onto your current branch (in the fork) and fast-forwards — linear
  history, no merge commits. Set `COOP_GATE` (e.g. `make check`) and it re-runs that
  gate in the box on the rebased tree, rolling back if it goes red — so "green" means
  green against your tree as it stands now, not the stale base the fork was cut from.
  `coop fork merge --all` lands the whole fleet as a revalidating rebase *queue*: each
  fork is rebased onto the result of the previous one and re-gated, stopping at the
  first conflict or failure and leaving the rest. **Commits get signed on land** if you
  sign (`commit.gpgsign=true`): the keyless box commits unsigned, then the rebase signs
  them with your host key (GPG or SSH) as it rewrites them — your signing key never
  enters the box.
- **Review and fleet management.** `coop fork review` now leads with a *brief* —
  commits, files changed, and the agent's own `.agent/LOG.md` reasoning — before the
  diff, so you get a map first. Review in your IDE instead of the pager with `--tool`
  (your configured `git difftool` — VS Code/JetBrains/Meld/vim), `--open` (open the
  fork in your editor — `$COOP_EDITOR`, then your `git config core.editor`, then a
  detected `code`/`cursor`/`zed`/`idea`), or override it entirely with `COOP_REVIEW_CMD`. `coop fork merge` runs a *policy check* that blocks
  secret-looking (`.env`, `*.pem`, `id_rsa`, …) or oversized files unless `--force`.
  Declare a fleet once in `.agent/fleet` (`<name> [agent]` per line) and drive it with
  `coop fleet up | ls | down`; `coop fleet split <n>` round-robins `.agent/TASKS.md`
  into per-fork slices to bootstrap one.
- **Every box run gets your git environment.** The box has no ambient `~/.gitconfig`,
  so an agent would otherwise commit with no author ("Author identity unknown") and
  ignore none of your global ignores. coop now mounts a curated `~/.gitconfig` into
  every run — your global `user.name`/`user.email`, your global gitignore
  (`core.excludesfile`), and `commit.gpgsign=false` (the box holds no signing key, so a
  global `gpgsign=true` would otherwise fail every commit). Forks additionally carry
  the parent repo's *local* identity (which a global mount can't see) and its global
  gitignore into the fork itself.
- **`coop claude|codex|gemini` now pass your extra args through.** They run the agent's
  autonomous default command *with any args you add appended* (the autonomous flags are
  global, so it's safe even before subcommands), instead of dropping the defaults — so
  `coop claude --continue`, `coop codex resume --last`, `coop gemini -p "…"`, etc. keep
  coop's autonomy + MCP wiring. `coop fusion` forwards extra args the same way.
- **`-h`/`--help` works on subcommands.** `coop fork [verb] --help` prints the fork
  family help, and `coop loop|up|down|init|doctor|build|update|dispatch|fleet --help`
  print the main help — all without needing a container runtime, instead of erroring
  (`unknown flag "--help"`) or running the command. (Agent commands still forward
  `--help` to the agent, so `coop claude --help` shows Claude's help.) Every short flag
  has a long form too (`-c`/`--continue`, `-d`/`--detach`, `-f`/`--force`/`--follow`),
  now shown in `coop fork --help`.

## 2.3.1

- **`--consult` makes the second opinion opt-in.** The peer-consultation directive
  introduced in 2.3.0 was always on; it now requires the `--consult` flag —
  `coop claude --consult` (or `codex`/`gemini`; in Zed, `coop acp claude --consult`).
  Off by default, otherwise unchanged: still injected only into the launched agent,
  still naming only the authenticated peers.

## 2.3.0

- **Agents can ask each other for a second opinion.** A normal `coop claude` (or
  `codex` / `gemini`) run now carries a light, optional directive: on a genuinely
  hard or risky call the agent may consult its peers **read-only and in parallel**
  to catch blind spots, then decide. It's injected only into the agent you launched
  (so peers it spawns don't recurse) and **names only peers that are authenticated**
  — if no other agent is logged in, nothing is added. The everyday, low-cost cousin
  of `coop fusion`, which mandates a full council + synthesis. Also covers
  `coop acp <agent>`; autonomous runs (`loop`, `dispatch`) are unaffected.

## 2.2.2

- **CI/CD supply-chain hardening.** Actions pinned to commit SHAs; CI runs with an
  explicit read-only token and no Actions cache (closes the cache-poisoning surface);
  `staticcheck` and the GoReleaser binary are version-pinned instead of tracking
  `@latest` / `~> v2`; checkout no longer persists the token in `.git/config`; release
  write scope is narrowed to the one job that needs it; and a Dependabot config keeps
  the pinned actions patched.
- `install.sh` now verifies the downloaded tarball against the release's
  `checksums.txt` and fails closed on a mismatch.
- **Signed, attested releases.** `checksums.txt` is signed keyless with
  Sigstore/cosign (a `checksums.txt.bundle`), and every archive carries a build
  provenance attestation — verify with
  `gh attestation verify coop_*.tar.gz --repo AndrewDryga/coop`. The repo also
  restricts Actions to an allowlist (GitHub-owned + goreleaser + cosign-installer)
  and requires approval for all outside-collaborator PR runs.

## 2.2.1

- **Bare `coop` now prints help instead of launching Claude.** Running an agent is
  explicit — `coop claude` (or `codex` / `gemini`) — so a stray `coop` never turns
  an autonomous agent loose on the current repo. `coop help` / `-h` are unchanged.
- **Per-language stacks dropped — `.tool-versions` is the single way to declare a
  toolchain.** `coop init --stack elixir|go|node|python` is gone; `coop init`
  auto-detects a `.tool-versions` and scaffolds the asdf `Dockerfile.agent` from it
  (`--stack asdf` forces it; a removed stack name now errors with a pointer to
  `.tool-versions`). The asdf image is a superset of the old per-language ones — it
  carries the build toolchain, `postgresql-client`, `procps`, and `inotify-tools`,
  and seeds `hex`/`rebar` when Elixir is present.
- The shared base box gains `postgresql-client`, `procps`, and `inotify-tools`, so
  the zero-config runtime path (bare `coop` on a repo with just a `.tool-versions`)
  matches a baked image. Run `coop update` to pull it.

## 2.2.0

- **`.tool-versions` honored by default — no `Dockerfile.agent` needed.** The base
  `coop-box` ships asdf and provisions a repo's `.tool-versions` toolchain at
  runtime (resolved from the cwd up the tree, or `~/.tool-versions`), cached in a
  shared `coop-asdf` volume so it installs once and is reused across repos. The
  first install of a toolchain can be slow (e.g. Erlang compiles), then it's
  instant. For a baked, reproducible image instead, `coop init` (or
  `--stack asdf`) scaffolds an asdf `Dockerfile.agent` that installs the same
  `.tool-versions` at build time. (`COOP_NO_ASDF=1` in agents/env opts out.)
- **`coop update`** rebuilds the box image fresh (`--pull --no-cache`) so the base
  image and the npm-installed agent CLIs + ACP adapters refresh to their latest
  (plain `coop build` is cache-bound and won't), then prints the resulting
  versions. The ACP adapters ship features often, so this is the easy way to stay
  current.

## 2.1.1

- Scaffolded stack images (`coop init --stack`) bake in the ACP adapters
  (`@agentclientprotocol/claude-agent-acp`, `@zed-industries/codex-acp`), so
  `coop acp` works in a project that has its own `Dockerfile.agent`. Without them
  it failed with `codex-acp: executable file not found`. An existing or
  hand-written `Dockerfile.agent` still needs the adapters added to its
  `npm install -g` line, followed by `coop build`.

## 2.1.0

- **Fusion mode — a governed council.** `coop fusion` runs one model as the
  *governor* (default `codex`; `--governor claude|gemini` or
  `COOP_FUSION_GOVERNOR`) and has it consult the other two **read-only and in
  parallel**, then synthesize the best result. It's a normal agent mode —
  interactive, headless, or in Zed via `coop acp fusion <governor>` (one
  `agent_servers` entry per governor to switch who leads). No extra service or
  MCP: the governor runs its peers (`claude -p --permission-mode plan`,
  `gemini --approval-mode plan -p`, `codex exec -s read-only`) from its own
  shell, and the fusion instruction is scoped to the governor only, so the peers
  it spawns never recurse.
- Smoother agent auth & first-run in the box. `coop login codex` uses the
  device-code flow (`codex login --device-auth`) — the container has no browser
  and codex's localhost OAuth redirect can't reach the host. Codex's "Do you
  trust this directory?" prompt is pre-answered (`[projects."<dir>"] trust_level =
  "trusted"`) and so is Gemini's folder-trust (`security.folderTrust.enabled =
  false`), matching Claude's first-run seeding — the box is the sandbox. All
  merged/idempotent, so an explicit choice and your other settings are kept.
  Interactive runs also propagate your terminal's `TERM`, so the agents' TUIs
  render in full color instead of warning about a basic terminal.
- Gemini no longer fails to launch on an empty `settings.json` ("Unexpected end
  of JSON input"); the box seeds valid JSON when that file is missing or blank
  (your own settings are preserved).

## 2.0.0

- **Renamed to Coop.** The binary is now `coop`, the image is `coop-box`, config
  lives in `~/.config/coop`, and env vars use the `COOP_` prefix (previously
  `agent-box` / `agent` / `AGENT_`). `install.sh` migrates an existing
  `~/.config/agent-box/agents` over on upgrade.
- The loop rides out rate limits. When the model hits a rate or usage limit
  mid-run, `coop loop` no longer spins on a fixed retry — it parses the reset
  time from the agent's own output (Claude's `usage limit reached|<epoch>`, or a
  `retry-after` delay), waits until then with a countdown, and resumes the same
  item, so an unattended overnight run survives the daily cap instead of burning
  retries against it. Non-limit failures back off and stop after a few in a row.
- Unified working directory and history across modes. `coop`, `coop loop`, and
  `coop acp` now all mount the repo at its real host path (not `/workspace`), so
  each agent's per-project session history is shared — a thread you started with
  `coop loop` is there to resume when you open the repo in Zed. `COOP_WORKDIR`
  still overrides the mount path for the old `/workspace` behavior.
- One-line install + releases: `curl -fsSL .../install.sh | sh` downloads the
  prebuilt binary (no Go, no clone). GoReleaser publishes cross-platform binaries
  — with auto-generated, categorized release notes — to GitHub Releases on every
  `v*` tag. CI runs gofmt, vet, staticcheck,
  tests, build, and shellcheck.
- Updated to current toolchain + base images: Go 1.26; GitHub Actions checkout
  v6, setup-go v6, goreleaser-action v7; the box base on node:24 (LTS); and the
  scaffolded stacks on python 3.14 / golang 1.26 / elixir 1.20-otp-29 /
  postgres 18 / redis 8.
- Rewritten in Go: `coop` is now a single static binary (no bash, no runtime
  dependencies) built with `go build`. Same commands, same box, same
  secret-shadowing — faithfully ported, with the security core (mount
  computation and run-arg assembly) now pure and unit-tested, and proven
  end-to-end by `coop doctor`.
- Native MCP generation: Gemini's `settings.json` and Codex's `config.toml` are
  produced in Go, so the host no longer needs `python3` for any agent's MCP.
- `coop init` templates and the workflow skills are embedded in the binary, so
  scaffolding needs no repo checkout (a step toward a fork-free install).
- Config moved to `~/.config/coop/` (XDG): `agents/`, `mcp.json`,
  `INSTRUCTIONS.md`, `env`, and `coop.conf` live there, decoupled from any repo.
  `install.sh` seeds it from an existing in-repo `agents/` on upgrade. The conf
  file is parsed as `KEY=VALUE` lines (the environment still wins over it).
- `coop help` and `coop version` no longer require a container runtime.
- Claude login now persists. The box sets `CLAUDE_CONFIG_DIR` to the mounted
  `~/.claude`, so Claude's account/onboarding state — which it keeps in
  `~/.claude.json` in `$HOME` — survives the disposable container instead of being
  lost every run (a latent bug from the bash version too; credentials already
  persisted, but without the config file Claude re-showed its login screen).
- Terminal detection uses a real isatty ioctl rather than a character-device
  check, so `coop run … < /dev/null` (and other char-device stdin) no longer
  wrongly requests a docker tty.
- Fresh boxes skip Claude's first-run prompts. Each run pre-answers the theme
  picker, the folder-trust dialog, and the bypass-permissions warning (the box is
  the sandbox) by seeding `settings.json` and `.claude.json` in the mounted config
  — merged, so the login and any settings you've chosen are preserved. A fresh
  install goes straight from one login to working.

## 1.6.0

- One file, every agent's MCP: define servers once in `agents/mcp.json` (the
  standard `{"mcpServers": {...}}` shape) and `coop` wires them into all three
  on launch — Claude via `--mcp-config`, Gemini merged into its `settings.json`,
  Codex converted to `[mcp_servers.*]` in its `config.toml`. The Gemini/Codex
  configs are generated read-only on top of your existing files (never modifying
  them; `mcp.json` wins on a name clash), which needs `python3` on the host;
  Claude needs nothing. `cp agents/mcp.json.example agents/mcp.json` to start.

## 1.5.0

- `coop acp [claude|codex|gemini]` — run the box as an ACP agent over stdio, so
  editors like Zed can drive the sandboxed agent. Mounts the repo at its real
  host path (so the editor's absolute paths resolve), attaches stdin without a
  pty, and keeps secrets shadowed. Point Zed's `agent_servers` at
  `command: "agent", args: ["acp", "claude"]`.
- The default image now bakes in the ACP adapters (`@zed-industries/claude-code-acp`,
  `@zed-industries/codex-acp`; Gemini's is built in) and trusts any git worktree
  (`safe.directory '*'`) so git works on the host-path mount.

## 1.4.0

- Ship generic workflow skills — `/plan`, `/work`, `/batch`, `/verify-api` — under
  `skills/`. `coop init` installs them into the repo's shared `.claude/skills/`
  (Codex gets them via the symlink), without clobbering ones you've edited, and
  the generated `AGENTS.md` points the agent at them. Adapted from the production
  set (emisar), with the repo-specific parts (Iron Laws, Elixir contexts) removed.

## 1.3.0

- `coop init` now scaffolds the full `.agent/` working set — `TASKS.md`,
  `BACKLOG.md`, `LOG.md`, `PENDING_DECISIONS.md`, `IDEAS.md`, `rules/` — and the
  generated `AGENTS.md` documents each one's role, so the canonical manual that's
  re-injected after a compaction tells the agent how to use them. Matches the
  layout used in production (emisar). Everything but `rules/` stays git-ignored.

## 1.2.0

- `coop dispatch <name>` — the fleet unit: clone into an isolated workspace,
  seed it with that agent's queue slice (`.agent/TASKS.<name>.md`), run the loop.
- `coop init` now wires the tool-neutral setup: `CLAUDE.md` and `GEMINI.md`
  symlink to the canonical `AGENTS.md`, and `.codex/skills` shares
  `.claude/skills`. Real (non-symlink) instruction files are never clobbered.
- Explicit `COOP_IMAGE` now overrides image selection (lets a dispatched clone
  reuse its origin's image); `COOP_SERVICES_NET` lets a fleet share one db/redis.
- Source-guarded entrypoint (`main`) so the script is unit-testable; added
  `test/unit.sh`, a `Makefile`, and CI (shellcheck + unit tests).

## 1.1.0

- Three agents in one box: `claude`, `codex`, `gemini`, each with autonomous
  defaults; `coop login <agent>`.
- Per-agent auth + settings in `agents/<agent>/`, mounted into the box; `agents/env`
  for API keys; one `agents/INSTRUCTIONS.md` wired into all three agents' native
  instruction paths, with per-agent overrides taking precedence.
- Per-project environments: `coop init --stack <elixir|python|go|node>` writes a
  `Dockerfile.agent` (toolchain) and `compose.agent.yml` (services); `coop up`/
  `down` run sibling Postgres/Redis the box reaches by name; per-project image tags.

## 1.0.0

- The box: `coop` runs a sandboxed agent on the current repo, with repo secrets
  shadowed out of reach (tmpfs over secret dirs, read-only decoys over secret files).
- `coop doctor` proves isolation by attacking it; `coop clone` hands off a
  secrets-free workspace with no reachable remote.
- The autonomous loop: `coop loop` works `.agent/TASKS.md` with disposable
  sessions and an audit pass; `coop init` scaffolds the queue + Stop/commit hooks.
