# Changelog

## Unreleased

<!-- Add entries here as you ship; this heading is renamed to the version on the next release. -->

- **`coop acp --supervise` no longer drops a request when the box restarts.** On a restart the
  proxy failed the in-flight requests (telling the editor to retry) a beat *before* it repointed
  to the new child, so a retry that arrived in that window was written to the dead child and
  silently dropped (a tight timing race; it also surfaced as an intermittently deadlocking test).
  The new child is now published before the retry signal, so the retry lands on it.

## 2.6.1

- **`coop doctor` works on rootful Linux Docker.** Its self-test probe was created mode 0600 and
  its fixture 0700; 2.6.0's new `--cap-drop ALL` strips `CAP_DAC_OVERRIDE` from the probe's
  root (alpine) container, so on rootful Docker it could no longer read its own probe and
  reported "the sandbox produced no output" (rootless podman and Docker Desktop remap ownership,
  so they were fine). The probe and fixture are now world-readable, and the check surfaces the
  container's actual error on failure. Real `coop run` was never affected — the box runs as
  non-root `node`, which never holds the capability either way.

## 2.6.0

- **`coop build` / `coop update` transparently restart supervised editor sessions.** They
  used to SIGKILL every running box (dropping a live editor session — Zed showed "Server
  exited with status 137"). Now they restart only **supervised** sessions (`coop acp
  --supervise`, tagged `coop.supervised`) onto the new image — the supervisor reconnects and
  replays the ACP handshake, so the editor doesn't notice — and leave everything else (a
  loop, a fork, an un-supervised session) running on the old image until it next starts.
  So after you edit `Dockerfile.agent` or `.tool-versions`, `coop build` moves your editor
  onto the rebuilt box without resetting the session. (The old `--restart` flag is gone.)
- **`coop acp --supervise` survives a box restart without the editor noticing.** Editors
  keep one ACP server process per agent and don't respawn a crashed one until you restart
  the whole editor. With `--supervise`, `coop acp` runs the agent in a child and proxies
  the connection; if the container dies (a rebuild, OOM, Docker restart), it starts a new
  one and replays the ACP handshake (`initialize`, `authenticate`, `session/load`), so the
  editor stays connected and the conversation resumes from the mounted home (verified
  end-to-end against the real claude + codex adapters: kill the box mid-session and the
  next prompt succeeds, still authenticated). Each supervisor tags its box `coop.sup=<id>`
  and kills exactly its own box on teardown — not other agents' supervised boxes. Opt-in; set
  it in your editor's args, e.g. `["acp","claude","--supervise"]`.
- **`coop init` scaffolds a commit gate that matches the repo's stack.** The pre-commit
  hook (and the Claude commit gate) used to hardcode a `gofmt` check — so a Terraform or
  Elixir repo got a dead Go gate and no gate for the language it actually uses. Now `coop
  init` detects the stack from go.mod / `*.tf` / mix.exs / Cargo.toml (or `.tool-versions`)
  and generates the matching format check: `gofmt`, `terraform fmt`, `mix format`, or
  `cargo fmt` — each `command -v`-guarded so it runs in the box and skips where the tool
  is absent. If it can't detect anything it leaves the gate **neutral** (no checks
  imposed) rather than guessing; at a terminal it asks which gate to add.
- **`coop init` suggests building the box on the repo's existing Docker.** When the repo
  already has a Dockerfile or compose file (and no `Dockerfile.agent` yet), `coop init`
  prints how to base the agent box on your image — the coop agent layer (node + the agent
  CLIs + a `node` user) to add on top — and how to reuse the compose services for `coop
  up`. Docs only: it lists the Dockerfiles, the compose services it found, and a ready
  snippet, but writes nothing.
- **Sibling services are opt-in.** `coop init` no longer drops a `compose.agent.yml`
  (Postgres + Redis) into every `.tool-versions` repo. At a terminal it asks which to add
  — none by default — or pass `--services postgres,redis`; a project that doesn't want a
  database isn't handed one. The file it writes carries only the services you picked.
- **`coop init` seeds an empty `mcp.json`.** It writes an empty
  `~/.config/coop/agents/mcp.json` (the shared MCP source of truth) so there's an obvious,
  correctly-shaped file to declare servers in — no more hunting for the path or format. An
  empty (no-server) `mcp.json` is now **inactive**, so the stub is a pure no-op until you
  add a server: an existing config is never touched, and runs are unchanged until you fill
  it in.
- **`coop init` output reads as a log plus next steps, not a wall of `coop:`.** The per-file
  progress (wrote/linked/added/gate) is now faint and unprefixed, a single `coop:` line
  summarizes, and the actions you take next — `coop build`, `coop up`, `coop loop`, each shown
  only when it applies — stand in their own spaced, colored "next steps" block. No more hunting
  for the three lines that matter among the twenty-five that don't.
- **Playwright works in the box.** Chromium's system libraries are now baked into the base
  image as root — the part an agent, running as the non-root `node` user, can't `apt-get`. The
  browser binary downloads to the cached `~/.cache` volume on first use, and the bundled
  `@playwright/mcp` example runs `--headless --no-sandbox` (the box is already the sandbox). So
  `npx playwright install chromium` + a `{ args: ['--no-sandbox'] }` launch — or the MCP server
  — drives a browser and takes screenshots instead of failing on a missing `.so`.

- **A live progress bar.** On a terminal, `coop loop` pins a Docker-build-style status bar to
  the bottom of the screen while it runs — a spinner, a progress bar, the done/total task count,
  the task in flight, and elapsed time — and the agent's activity scrolls above it. The bar
  tracks the queue as the agent rewrites it, so progress moves live within an iteration, and the
  run is bracketed with starting and finishing tallies. Piped or CI output stays plain.
- **Watch the agent work.** On a terminal, `coop loop` with Claude renders the agent's activity
  live instead of going dark until the iteration ends — each tool call (`✎ Edit`, `⚙ Bash`,
  `▸ Read`), the agent's own narration (marked `✦`), and a closing `· N turns · time · $cost` —
  by decoding Claude's stream-json output. coop's own lines wear a bold-cyan `coop:` so its
  voice stays distinct from the agent's in the scroll. Multi-subscription failover keeps working underneath (the
  structured rate-limit signal is translated for the detector). Other agents, a custom
  `COOP_LOOP_CMD`, and piped/CI runs keep plain text output.
- **Watch the whole fleet.** `coop fleet watch` (or `coop status --watch`) turns the fleet
  roll-up into a live dashboard — one row per fork with a spinner, a progress bar, the task in
  flight, and its last log line, plus a global progress bar across every fork's tasks — refreshing
  in place. It polls the same queue/log/pidfiles `coop status` reads (no daemon); Ctrl-C exits.
  A non-terminal falls back to a single `coop status` snapshot.
- **Multi-subscription failover for the loop.** One agent can now hold several accounts
  as named profiles — `coop login claude --profile work` — and when the unattended loop
  hits a rate/usage limit it switches to another signed-in profile and keeps going, only
  waiting once every profile is limited. So a long run rides through a (multi-day)
  subscription cap instead of parking on it. With no setup it rotates across all of an
  agent's signed-in profiles; `coop pool add <agent> <profile…>` narrows a repo to a
  chosen set, and `coop profiles` lists what you have. `coop profiles default <agent>
  <name>` marks which profile an interactive `coop claude` uses, so the default is a
  mark you set rather than whichever profile happens to be named "default". Profiles live
  in the vault
  (`~/.config/coop/agents/<agent>/profiles/`), never in the repo, and only the active one
  is mounted into the box — a running agent sees just the account it's using, not the
  whole vault. Existing single logins are untouched: they become the "default" profile in
  place, with no migration, and behave exactly as before.
- **Configurable, multi-file task queues.** `coop tasks` and `coop loop` take a repeatable
  `--tasks <path>` (or `COOP_TASKS`, space-separated), defaulting to `.agent/TASKS.md`. A
  monorepo with a queue per component can now inspect or drain several at once: `coop tasks
  list --tasks portal/.agent/TASKS.md --tasks runner/.agent/TASKS.md` aggregates them under
  per-file headers, and `coop loop --tasks portal/.agent/TASKS.md --tasks runner/.agent/
  TASKS.md` works the union until every file is drained — one loop covering several
  components, with the whole repo still mounted. list and lint span all the files; add and
  split target a single one. Paths are relative to the repo root.
- **Host-side git is hardened against a poisoned repo.** coop bind-mounts your repo into the
  box with its `.git` writable, so a prompt-injected agent could plant git config that runs a
  command on *your* host the next time coop touches the repo — a `core.fsmonitor`, a hook,
  `diff.external`, a `gpg.program`, a filter/merge/diff driver. Every host-side git call coop
  makes now blanks those exec-bearing knobs, and any config coop reads then executes or reads a
  host file from (your editor, signing program, excludesfile) is read from your **global** git
  config, never the agent-writable repo. `coop check-secrets`, `coop fork merge`/`review`, and
  `coop status` are all covered, each with a poisoned-config test that fires a raw-git positive
  control so the guard can't silently rot.
- **The box drops privileges and can run fully offline.** Every box now starts with `--cap-drop
  ALL` and `no-new-privileges` (an agent needs neither), so a repo `Dockerfile.agent` that does
  `USER root` can't regain `NET_RAW`/`MKNOD`/etc. New opt-in `COOP_EGRESS=none` runs the box
  with no network at all — for a run you don't trust. The secret-shadow denylist gained common
  service-credential names, and the README now states plainly that **`.coopignore`, not
  `.gitignore`, is the boundary** for what the agent can read on a bind-mounted run.
- **`coop fork merge` defends the host on land.** Landing a fork runs git on your machine, so a
  fork that planted an execution-on-interaction file (`.envrc`, `.vscode/tasks.json`, a new
  `Makefile`, a `package.json` that adds an install script) or neutralized its `.gitattributes`
  drivers is flagged and blocked without `--force`; an untracked `Dockerfile.agent` (which
  defines the box an agent could author) is flagged before `coop build`. And `coop fork merge
  --all` now asks before it lands and **deletes** every fork — it used to do that with no
  confirmation at a terminal.
- **CLI papercuts, fixed.** `coop run --help` and bare `coop run` print usage instead of
  crashing the box; `coop login` requires the agent and refuses a non-interactive stdin; `coop
  help <cmd>` shows that command's page; every command has a real `--help`; a bad
  subcommand/agent/flag is rejected the same way everywhere, with a "did you mean…?";
  `$COOP_RUNTIME` is validated up front (a clear "runtime not found", not a misleading "image
  not built"); and the `coop status` / `coop fork ls` / `coop profiles` tables size their
  columns to the data, so a long fork name no longer breaks the alignment.
- **Safer fork loops.** `coop fork` refuses to start a second loop on a fork that's already
  looping (it was overwriting the pidfile, orphaning the first worker and leaving two loops
  racing the same worktree), and `coop fleet up` skips already-running forks so a re-run is
  idempotent. An empty `COOP_CLAUDE_CMD` / `COOP_GEMINI_CMD` override now degrades to the bare
  CLI instead of producing a command with no executable.

## 2.5.2

- **`coop acp` runs quiet.** ACP speaks to an editor over stdio, so coop's progress lines
  (the secret-shadow count, the fusion-governor note) and the box's toolchain-provisioning
  chatter were just noise in the editor's log. ACP now suppresses them — the toolchain
  still provisions, silently (`COOP_QUIET`). Other modes are unchanged.
- **Consistent `coop:` log prefix.** coop's progress/error lines used a dimmed `agent:`
  prefix while the box's provisioning printed `coop:` — now both are a dimmed `coop:` (the
  tool's own name), so output reads as one voice.
- **The box only narrates provisioning when there's actually something to install.** The
  "provisioning toolchain…" line (and asdf's "already installed" output) printed on every
  launch even when every pinned tool was already present — pure noise. The entrypoint now
  checks `.tool-versions` against what's installed and stays silent unless a tool is
  missing.
- **`coop check-secrets` no longer scans vendored or gitignored files.** It enumerates the
  working tree the way git does — tracked plus untracked, gitignored paths excluded —
  instead of walking everything. Build output and dependencies (`node_modules/`, `dist/`,
  `_build/`) are skipped, which is where the bulk of the false noise lived: across the
  author's projects one app dropped from ~1,900 hits to a handful.
- **The secret scanner flags literal credentials, not references to them.** The entropy
  heuristic was tripping on ordinary code and config, not just secrets. It now requires the
  key to *end* in a credential word (so `authenticator`, `token_url`, `allocate_tokens`
  no longer match) and skips values that are references rather than literals: variable and
  config refs, `${…}`/`{{…}}` interpolations, function calls, Rust generics/namespaces,
  Elixir module attributes, `SCREAMING_SNAKE` constants, comments, URLs and paths, and
  obvious placeholders or fixtures (AWS `…EXAMPLE` keys, `"very-long-password-1234"`). A
  real random token contains none of these, so a genuinely committed secret still surfaces.
  Shared by `coop check-secrets` and the `coop fork merge` policy.

## 2.5.1

- **Secret directories are now shadowed on Podman too.** Directory shadowing used
  `--tmpfs`, which Podman applies in a separate pass from `-v` binds — so the repo bind
  re-covered it and `secrets/`-style directories were re-exposed inside the box (file
  shadowing via the read-only decoy was unaffected, and Docker was unaffected because it
  sorts all mounts by destination). Now a directory is shadowed by a read-only empty-dir
  bind, which sorts with the repo bind on every runtime. `coop doctor` passes on Podman.

## 2.5.0

- **`coop check-secrets` — content secret scan of the visible tree.** Shadowing hides
  secrets by filename; this catches a token hiding *inside* an ordinary file. It walks the
  non-shadowed working tree (exactly what the box can see), runs the same content scanner
  the fork-merge policy uses, and reports each as `file:line`, exiting non-zero on a hit
  (pre-flight / CI). The shadow rule is now one shared `box.NewShadowDecider`, so the
  scanner and the mount plan can't disagree about what the box sees.
- **`coop tasks` — `.agent/TASKS.md` as a validated surface.** `list` shows states and
  titles, `lint` flags stale `[w]` claims / tasks missing the self-contained five-part
  shape / unchecked tasks stranded in `## Example` / malformed markers (exit 1 on
  findings, for pre-flight or CI), `add "<title>"` appends a well-shaped stub, and
  `split <n>` carves the queue into slices. All run through one anchored parser shared
  with the loop, fleet split, and status — so `coop fleet split` now carries each task's
  whole body into its slice (slices stay self-contained) instead of bare title lines.
- **`coop fleet init`.** Writes a documented `.agent/fleet` template — the
  `<name> [agent] <tasks-path>` format explained in inline comments — so you can declare a
  fleet without looking up the syntax. Refuses to clobber an existing file.
- **`coop status` + a richer `coop fork ls`.** Watching a fleet loop overnight meant
  tailing N logs. `coop status` now rolls the fleet up at a glance — per fork: running or
  idle, tasks done/total, blockers, diff size, and the task it's on — plus fleet totals.
  `coop fork ls` gains a tasks-progress column. Both read existing sources (the fork's
  queue, git, the loop pidfile); no daemon. The anchored TASKS.md parser is shared, so the
  loop, fleet split, and status can't drift apart.
- **Aligned `coop fork ls` / `coop status` tables.** Bold column headers no longer count
  their (invisible) ANSI escape bytes toward the `%-Ns` column width, so on a terminal the
  header lines up with the rows beneath it instead of drifting left.
- **Unknown commands fail fast instead of being run in the box.** `coop <typo>` used to be
  shipped into the box and die with a cryptic `exec: …: not found` after a slow toolchain
  spin-up. An unrecognized command now errors immediately with a "did you mean …?"
  suggestion and a reminder that raw box commands are explicit. This drops the implicit
  `coop npm test` passthrough — run raw commands with `coop run -- npm test`.
- **`coop <command> --help` shows that command's own help.** Every subcommand's `--help`
  used to print the generic usage; now it prints focused help — synopsis, what it does,
  and its flags/subcommands. (`coop fork` keeps its fuller help; `coop run` and the agents
  still forward `--help` to the underlying command.)
- **`coop <command> help` works, and no-argument commands reject stray args.** `coop build
  help` (or any extra token) used to be silently ignored — the build just ran. Now a
  positional `help` prints the command's help like `--help`, and the no-argument commands
  (`build`, `update`, `doctor`, `status`, `up`, `shell`, `check-secrets`) reject unexpected
  arguments with a clear error instead of ignoring them.
- **`coop help` is a clean command reference.** Reworked into one-command-per-line groups
  (fork verbs are listed individually, not collapsed into `<verb>`), fixed long commands
  gluing onto their descriptions, and dropped the verbose prose tutorials — each command's
  flags and examples now live in its own `coop <cmd> --help`.
- **`coop init` installs a git pre-commit gate for every committer.** The scaffolded
  `.claude/hooks` only fire for Claude, so Codex/Gemini and plain `git commit` bypassed
  the format gate. `init` now also writes a tracked `.githooks/pre-commit` (gofmt-checks
  staged files, fails closed) and sets `core.hooksPath=.githooks`, so the gate runs for
  everyone and travels with a fresh clone. A custom `core.hooksPath` is never clobbered;
  `git commit --no-verify` skips it.
- **Stale-image warning.** A per-project image (built from `Dockerfile.agent`) bakes the
  repo's toolchain at build time, so it can drift from the files that define it. `coop`
  now records a hash of `Dockerfile.agent` + `.tool-versions` at `coop build` time and, on
  a later interactive run, warns when they've changed so you remember to rebuild. (The
  shared base is exempt — `coop update` keeps it fresh.)
- **`coop loop --debug-on-fail`.** When an iteration fails at a terminal, the loop opens
  an interactive shell in the box (same repo + image) instead of auto-retrying or
  stopping — inspect files/env/run the gate, then exit the shell to retry (Ctrl-C to
  stop). A no-op in non-interactive/detached runs.
- **`.coopignore` works in subdirectories.** Like `.gitignore`, a `.coopignore` in any
  directory is scoped to that subtree (basename patterns at any depth under it, path
  patterns relative to it), so a monorepo's sub-teams can keep folder-local shadow rules
  next to their code instead of all in the repo root.
- **`coop fork merge` scans changed content for secrets, not just filenames.** The
  policy check now reads each changed blob and flags real credentials — provider token
  shapes (AWS/OpenAI/Anthropic/GitHub/Slack/Google/Stripe, private keys) and high-entropy
  values on secret-named keys — so a token committed inside an ordinary `config/prod.yml`
  is caught even though its filename is innocuous (override with `--force`).
- **The box caps a runaway agent.** Runs now set a `--pids-limit` (default 4096, a
  fork-bomb cap) and `--security-opt no-new-privileges`, with optional `--memory` /
  `--cpus`, so an agent in a loop can't fork-bomb or starve the host. Tunable via
  `COOP_PIDS` / `COOP_MEMORY` / `COOP_CPUS` / `COOP_NO_NEW_PRIVILEGES`. Applied on docker
  and podman; Apple's `container` CLI differs, so it's skipped there for now.
- **`coop fleet split` no longer creates phantom tasks.** It slices only real `- [ ]`
  task lines (the same anchored rule the loop uses), so the TASKS.md legend or an
  `## Example` block can't become a fake item in a fork's queue.

## 2.4.0

- **Fusion mode consults on every task.** The governor's directive is now
  unconditional — no "trivial change" or "I already know it" exception — so a
  fusion governor always consults both peers before answering or acting (only
  incidental shell like `ls`/`git status` is exempt, as it isn't itself a task).
- **In-box agents no longer trip over the absent OS sandbox.** The box is itself the
  sandbox and ships no bubblewrap, so coop tells Claude Code to skip subprocess
  env-scrubbing (`CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0`) and pins its bash sandbox off
  in settings (`sandbox.enabled=false`, `failIfUnavailable=false`); the shared
  `INSTRUCTIONS.md` now notes a missing sandbox is expected, not a bug. Codex and
  Gemini already launch unsandboxed (`--dangerously-bypass-approvals-and-sandbox`,
  `--yolo`), so they needed no change.
- **`coop clone` is now `coop fork` — a full local-PR lifecycle, not a one-shot
  handoff.** A fork is a throwaway local clone (own `origin`, nowhere to push, no
  gitignored secrets); treat it like a contractor's PR — open, review, merge, close:
  - `coop fork <name> [claude|codex|gemini]` — open or re-enter a fork and run the
    chosen agent. **A fork remembers its agent** (persisted git-excluded inside the
    fork), so re-entering without one continues with the model it was created with, not a
    silent fallback to claude. **Re-entering also continues your last session by default**
    (the fork's history persists): claude `--continue`, gemini `--resume latest`, and
    codex by the session whose recorded `cwd` is that fork — each **scoped to this fork**
    so it never resumes an unrelated session. Falls back to fresh when none exists;
    `--new` forces a fresh session.
  - `coop fork ls` — list this repo's forks with agent, branch, change size, last activity;
    `coop fork open <name>` opens it in your editor, `coop fork path <name>` prints its path.
  - `coop fork review <name>` — fetch the fork's branch into `review/<name>` and show
    the diff (no more hand-typed `git fetch … && git diff`).
  - `coop fork merge <name>` — land it by **rebasing** the fork onto your branch
    (linear history, no merge commit), then offer to close it; refuses if your tree
    is dirty.
  - `coop fork rm <name>` — discard a fork; refuses while its work is unmerged or
    dirty unless `--force`.
  Forks live in a sibling `../<repo>-forks/` (was `-agents/`). `coop clone` stays a
  back-compat alias. **`coop dispatch` is removed** — it was a single fork with an
  implicit `TASKS.<name>.md` mapping, now fully covered by
  `coop fork <name> <agent> --loop --tasks <path>`.
- **A fleet of forks, each on a different model, looping in the background.**
  `coop fork <name> <agent> --loop --tasks <path>` runs the unattended loop in a fork
  with the chosen model — claude (`-p`), codex (`exec`), or gemini (`-p`) — seeding its
  queue from the tasks file you name. `--tasks` is **required and explicit** (no implicit
  `TASKS.<name>.md` mapping), so a fork and its tasks file are named independently. Add
  `-d` to detach it (session-leader background worker; logs captured to
  `../<repo>-forks/.coop/<name>.log`). New process commands: `coop fork logs [name] [-f]`
  (no name = every fork at once, prefixed), `coop fork stop <name>`, and a running/idle
  column in `coop fork ls`. Declare a fleet in `.agent/fleet` as
  `<name> [agent] <tasks-path>` per line — `coop fleet split` writes that file for you.
- **`coop loop` takes a model.** `coop loop [claude|codex|gemini]` runs the main
  unattended loop with the agent you pick (default `claude`), instead of always claude;
  `COOP_LOOP_CMD` still overrides the iteration command outright.
- **Fewer silent defaults.** `coop fork merge` prints which branch it's landing onto (it
  rebases onto your *current* branch); a fork `--loop` that already has a queue says
  `--tasks` isn't being re-applied (use `--fresh` to reseed) instead of dropping it
  silently; `coop acp` names the model it defaulted to / which governor leads under
  fusion. `coop fleet ls` is gone — it was a pure alias for `coop fork ls`.
- **`coop fusion <agent>` picks the governor positionally** — `coop fusion claude`, to
  match `coop acp fusion claude`. The `--governor` flag is removed (use the positional
  form or `COOP_FUSION_GOVERNOR`).
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
  Declare a fleet once in `.agent/fleet` (`<name> [agent] <tasks-path>` per line) and
  drive it with `coop fleet up | down` (list with `coop fork ls`); `coop fleet split <n>`
  round-robins `.agent/TASKS.md` into per-fork slices and writes a matching `.agent/fleet`.
- **Drive a fork (or the project) from Zed (ACP).** `coop fork <name> acp [agent]` fronts
  a fork as an ACP agent over stdio (pinned to the fork's path and the parent's image);
  `coop acp [agent]` does the same for the project in your cwd. Register them once in your
  *Zed user settings* (`agent_servers` is user-level — Zed rejects it in a project's
  `.zed/settings.json`); since `coop acp` resolves the repo from its cwd, one set of entries
  covers every coop project you open, forks included (`coop fork open <name>` opens a fork
  in your editor). Worktree Trust still applies; resuming a prior session rides on ACP
  `session/load`, which the editor drives.
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
  family help, and `coop loop|up|down|init|doctor|build|update|fleet --help`
  print the main help — all without needing a container runtime, instead of erroring
  (`unknown flag "--help"`) or running the command. (Agent commands still forward
  `--help` to the agent, so `coop claude --help` shows Claude's help.) Every short flag
  has a long form too (`-c`/`--continue`, `-d`/`--detach`, `-f`/`--force`/`--follow`),
  now shown in `coop fork --help`.
- **Host-side git in a fork can't be hijacked by the agent.** A fork is agent-writable
  down to its `.git/`, yet `coop fork merge`/`review`/`ls`/`rm` run host git in it. Those
  calls now disable hooks (`core.hooksPath=/dev/null`) and blank every config knob that
  shells out (`core.fsmonitor`, a forced `commit.gpgsign` + `gpg.program`, …), so a
  planted `.git/hooks/*` or malicious `.git/config` can't execute on your host. Signing on
  land reads your *parent* repo's signing config, never the fork's.
- **`.coopignore` hides repo-specific secrets.** Secret shadowing matched a built-in
  denylist by basename, so a committed `config/credentials.yaml` stayed visible to the
  agent. Add a repo-root `.coopignore` — basename patterns (any depth) or repo-relative
  path patterns — to shadow anything else. The defaults also grew (`*.keystore`, `*.p8`,
  `*.ppk`, `*.kdbx`, `*.ovpn`, `id_dsa*`, `.htpasswd`, `.dockercfg`, `.pgpass`); prove your
  setup with `coop doctor`.
- **`coop fork merge` won't land non-interactively without `--yes`.** `confirm()` returned
  its default with no TTY, so in CI or a pipe a merge would land work and delete the fork
  unprompted. It now refuses unless you pass `--yes` (which also skips the prompts
  interactively); `--all` is covered too.
- **`install.sh` verifies the release *signature*, not just the checksum.** When `cosign`
  is on `PATH` it verifies the Sigstore signature on `checksums.txt` (via
  `checksums.txt.bundle`) and fails closed before trusting it — so swapping both the
  archive and its checksum file is caught. Without cosign it keeps the SHA-256 check and
  says the signature wasn't verified; the README documents manual verification.
- **`coop build` is reproducible; `coop update` stays fresh.** The base image `FROM` is
  pinned to a specific `node:24` digest, so a `coop build` gets the same OS/runtime every
  time; `coop update` floats it back to the tag and rebuilds `--pull --no-cache` for the
  latest. Pin the agent CLI / ACP npm versions too with `COOP_AGENT_PACKAGES`.
- **The box ships `ripgrep`, `fd`, `jq`, and `tree`.** The search/inspect tools agents
  reach for constantly are baked into the base image (`fd` is symlinked from Debian's
  `fdfind`). Run `coop update` to pick them up.
- **The shared cache volume is writable by the agent.** `coop-cache` mounts at
  `/home/node/.cache`, but the path wasn't pre-created in the image, so a fresh volume came
  up root-owned and Go/npm/pip builds hit `permission denied`. The base and scaffolded
  images now pre-create it `node`-owned. Repair an existing volume with
  `docker volume rm coop-cache`, then rebuild.
- **The loop waits out Claude's *weekly* limit too.** `coop loop` already parsed
  `usage limit reached|<epoch>` and `retry-after` delays; it now also recognizes the
  current notice — `You've hit your weekly limit · resets Jun 18, 8pm (UTC)` — parses that
  reset and sleeps until it (a multi-day wait if need be), instead of mistaking it for a
  plain failure and stopping after a few retries.
- **`coop loop` detects an empty queue correctly.** Its todo scan matched any `[ ]`
  substring, so the legend line in `.agent/TASKS.md` always counted as work — the loop
  could never reach "queue empty" and the Stop-hook saw a phantom item on a finished queue.
  It now counts only real `- [ ]` task lines.
- **`coop login` re-opens the sign-in flow when you're already logged in.** It runs
  `claude auth login` (was a bare `claude`, a no-op once authenticated), so you can
  re-authenticate or switch accounts — e.g. off a rate-limited one. `coop <agent> login`
  works as well as `coop login <agent>`.
- **Command settings honor shell quoting.** `COOP_GATE`, `COOP_LOOP_CMD`, `COOP_RUN_ARGS`,
  and the `COOP_<AGENT>_CMD` overrides split with shell quoting (quotes group, `\` escapes)
  — without running a shell — so `COOP_GATE='bash -lc "make check && make lint"'` is three
  args, not five.
- **`coop init` scaffolding refinements.** Workflow skills now live once in
  `.agent/skills/`, with `.claude`/`.codex`/`.gemini` each symlinking to it (replacing
  three drifting copies and an orphaned root `skills/`). The scaffolded `.agent/` files
  model their own shape with an `## Example`, `TASKS.md` starts with an empty queue, and
  the `AGENTS.md` contract gains rules: tasks must be self-contained (workable from the
  BOOT files alone), don't create git branches unless asked, and `IDEAS.md`/`BACKLOG.md`
  hold a dump of your current thinking (spec included), not triage notes.

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
