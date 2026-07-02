# Changelog

## Unreleased

<!-- Add entries here as you ship; this heading is renamed to the version on the next release. -->

- **`:d` in `coop tasks decisions -i` marks a task done.** When a blocked decision's real answer is
  "already handled," you no longer have to answer → unblock → claim → done: type `:d` (or `:d
  <reason>` to record why in `decision.md`) to move the task straight to `99_done/`. The walker's key
  legend is colorized to match.

- **Clearer errors, and consistent split-slice naming.** `coop up` with no compose now points at
  `coop init --services postgres,redis` (not `--stack`, which only scaffolds an asdf Dockerfile);
  a missing `.agent/fleet` points at `coop fleet init`; and `coop tasks split` now names its slices
  `.agent/tasks.slice<n>` to match `coop fleet split` (the two were `tasks.<n>` vs `tasks.slice<n>`).

- **`coop doctor` warns loudly when it's probing an alpine stand-in.** With no box image built,
  doctor falls back to a stock `alpine` (skipping the non-root USER and toolchain checks) — it only
  disclosed this in a dim aside, so a newcomer read a green bill of health and then hit a failing
  `coop claude`. It now prints a yellow ⚠ telling you to run `coop build` and re-run. The docs that
  claimed `coop doctor` "builds the box" (it never did) are corrected.

- **BREAKING — `coop loop` exits 3 when it stops with work blocked on a human decision.** Every loop
  outcome used to exit 0, so cron/fleet/CI couldn't tell "queue drained" from "stalled on a blocked
  task" without scraping stderr. The exit contract is now `0` verified done (or the audit reopened
  work — re-run); `1` failure; `2` usage; `3` stopped with a task in `50_blocked/` and nothing else
  actionable. A script that treated any non-zero loop exit as failure should special-case 3.

- **Stdout views stay clean when piped.** `coop help`, a command's `--help` page, `coop profiles`,
  `coop models`, `coop fork ls`, and `coop loop pool` colored their output through the stderr-gated
  helpers, so `coop profiles | grep` or `coop fork ls | wc -l` from an interactive shell received raw
  ANSI escapes. They now gate color on stdout (via `ui.For(os.Stdout)`), so a pipe or redirect gets
  plain text.

- **Scaffolded `postgres:18` services start out of the box.** `coop init --services postgres`
  mounted the data volume at `/var/lib/postgresql/data`, which the postgres 18+ image refuses — the
  container exited 1, and auto-up (`COOP_AUTO_UP=1`) silently continued without the database. The
  scaffold now mounts `pgdata` at `/var/lib/postgresql`. Already scaffolded before the fix? Move the
  mount up one level (see Troubleshooting).

- **`coop loop` no longer reports "queue verified done" when the audit reopened work.** The
  end-of-run audit reopens failed done tasks by moving them into `10_in_progress/`, but the closing
  banner checked `00_todo/` only — so it printed a green "verified done" over a queue that still had
  reopened work, and an unattended user (or a wrapper keying off the message) walked away. It now
  counts everything actionable and prints `⚠ audit reopened N task(s) — run 'coop loop' to work them`.

- **`coop tasks path <id>` prints a task's resolved folder.** The companion to `coop fork path`:
  resolve a task by id or slug fragment and print its directory, so `cat "$(coop tasks path
  <id>)/task.md"` works from a shell or hook instead of hunting the four state dirs by hand. It
  spans configured queues like the other id commands.

- **Security — fork commands reject a name that escapes the forks directory.** `coop fork rm ..`
  `filepath.Join`-cleaned the name before deleting, so `..` resolved to the parent of all your
  projects and `os.RemoveAll` wiped it — a live, unrecoverable data-loss bug. Every name-taking
  fork verb (`rm`, `stop`, `open`, `logs`, `review`, `path`, `merge`, and `--fresh`) now validates
  the name up front and refuses anything that isn't a single safe path segment.

- **The rotation pool lives under the loop: `coop loop pool add|rm|clear`.** The pool is a
  *setting* of the loop (which subscriptions it rotates on a rate limit), not a workflow of its
  own, so it no longer sits beside `coop loop` as a top-level command. Same verbs, same storage;
  `coop pool` still works as an undocumented alias, so nothing breaks.

- **`coop loop` keeps the machine awake while it runs.** An overnight drain is pointless if the
  laptop idle-sleeps midway through it, so the loop now holds a system sleep inhibitor for its
  duration — macOS `caffeinate -i -m -s`, tied to coop's own process (`-w`) so it self-releases
  even on a hard kill, and released the moment the loop returns. Best-effort (a missing tool just
  runs the loop unchanged) and on by default; set `COOP_CAFFEINATE=0` to opt out. Covers `coop
  loop` and fork loops (foreground and detached). The display is deliberately left free to sleep.

- **BREAKING — tasks are folders now (`.agent/tasks/`); the single `.agent/TASKS.md` is gone.**
  The work queue is one folder per task under four state directories — `00_todo/` ·
  `10_in_progress/` · `50_blocked/` · `99_done/` — and a task's workflow state is simply which
  directory it sits in. There is no `status:` field and **no legacy fallback**: coop no longer
  reads a single-file `TASKS.md`.

  *Why it's better.* A finished task physically leaves the queue (its folder moves to
  `99_done/`), so the "done but never pruned" rot that bloated a single `TASKS.md` is gone and
  the loop never re-scans shipped work. State can't drift from reality, because the directory
  *is* the state — every transition is an atomic folder move (`os.Rename`) in `.agent/tasks`,
  which `coop init` keeps as gitignored local working state (not versioned). Each task carries
  its own design (`spec.md`), working journal (`log.md`), resume note
  (`state.md`), pending decision (`decision.md`, replacing the global `PENDING_DECISIONS.md`),
  and `screenshots/`/`artifacts/` instead of everything piling into shared top-level files. The
  numeric directory prefix is a pure sort key, so a plain `ls .agent/tasks` lists the states in
  lifecycle order (todo → in_progress → blocked → done) rather than alphabetically (`99_` keeps
  done last); `coop tasks` still prints the clean names. `coop tasks` drives it all —
  `add`/`claim`/`block`/`unblock`/`done`/`rm`/`ls`/`lint`/`decisions` — and the loop, `coop fleet`,
  the Stop hook, and `coop init` are folder-native. Subtasks are a `- [ ]`
  checklist inside `task.md`. A finished
  task is **moved** to `99_done/`, never deleted: the loop and `/sweep` only ever move tasks
  between states, so done tasks accumulate as the shipped record until you prune them by hand with
  `coop tasks rm --all-done` (or `coop tasks rm <id>` for one).

  *Migrating.* It's a one-time, content-preserving conversion an LLM handles well (the old task
  bodies are prose to map, not a rigid parse). Commit first, then paste the prompt below to any
  coding agent **running in the repo**; afterward verify with `coop tasks` and `coop tasks lint`.
  The full version (with the `decision.md` / `PENDING_DECISIONS.md` handling spelled out) is in
  [`MIGRATING.md`](MIGRATING.md):

  ```text
  Convert this repo's legacy coop task queue to the folder format; lose no content.
  SOURCE: `.agent/TASKS.md` — each top-level `- [ ] / [w] / [x] / [B] <title>` is one task and
  the indented bullets beneath it are its body; `.agent/PENDING_DECISIONS.md` holds decisions.
  Ignore the legend, the `[E]` example, and any `- [ ]` lines inside ``` fenced code blocks.
  TARGET: a folder per task; the task's STATE is its directory (use the NN_ prefix verbatim):
    `- [ ]` -> `.agent/tasks/00_todo/`      `- [w]` -> `.agent/tasks/10_in_progress/`
    `- [B]` -> `.agent/tasks/50_blocked/`    `- [x]` -> `.agent/tasks/99_done/`
  For each task write `.agent/tasks/<state>/<YYYY-MM-DD-slug>/task.md`: frontmatter (id, title,
  labels, updated) + the body mapped into **Context** / **Acceptance criteria** / **Approach**
  and a `## Subtasks` checklist; never add a `status:` field. For a `[B]` task also write
  `<id>/decision.md` (question, options, recommendation, resolution), folding in any matching
  `PENDING_DECISIONS.md` entry. Then delete `.agent/TASKS.md` and `.agent/PENDING_DECISIONS.md`,
  and report the tasks migrated per state.
  ```

- **New `coop tasks watch`; `coop status` removed.** `coop tasks watch` is a live, task-centric
  board — the queue itself (in progress / todo / blocked) draining in place, with overall progress;
  it auto-exits only when every task is done, and keeps watching a blocked or idle queue. With a
  fleet running it merges in every active fork and the tasks it claimed — one view, deduped by task
  id, with each in-progress task tagged by the fork on it — so it's the single place to see all the
  work and who's doing what. The fleet's per-fork board stays at `coop fleet watch` (snapshot:
  `coop fork ls`). `coop status` is gone: it was a third entry point that overlapped `coop fork ls`,
  and its `--watch` was a straight alias for `coop fleet watch`. Listing is normalized too — `ls` is
  the canonical verb everywhere (`coop tasks ls`, `coop fork ls`), with `list` kept as a forgiving
  alias, matching `rm`/`remove`.

- **The loop hands off mid-task through a per-task `state.md`.** Each `coop loop` iteration runs a
  fresh headless agent with no memory of the last, so a task interrupted mid-flight used to be
  resumed only by reverse-engineering the uncommitted `git diff`. Now the work prompt has the agent
  keep a small, overwritten resume note in the in-progress task's folder (`state.md`: status ·
  what's done · next action · traps), refreshed at each checkpoint, and read first when resuming
  it. Because it's
  plain markdown in the repo, the next iteration can be a *different* agent (claude → codex → gemini)
  and still resume cleanly — the groundwork for switching agents, not just credential profiles,
  between iterations. The agent finalizes the note as its last step on a task — even when done — so
  a review can reopen it and the next agent picks up the requested changes from the note rather than
  the diff.

- **Self-documenting task files, and a first-class blocked-decision flow.** `coop tasks add` seeds
  `task.md` + `log.md` + `state.md` (and `coop tasks block` a `decision.md`), each opening with a
  short header that explains the file and its format — so a resume snapshot, the *why* journal, and
  a blocked one-way-door decision are all by-the-book without leaving the folder. The `task.md`
  header directs the agent that picks the task up to replace its `<…>` placeholders (Context /
  Acceptance / Approach) *before* writing code, or block it — so a vague title can't be coded
  against blind. A blocked decision is then resolved from the CLI: `coop tasks decisions` lists the
  open ones with their full recommendation, or `-i` walks them one at a time to answer in place;
  answering — inline with `coop tasks unblock <id> "<answer>"` or at the interactive prompt —
  records the answer into `decision.md` and returns the task to `todo` for the loop to pick up.
  `.agent/tasks/README.md` is the full reference: a description, template, and worked example for
  every per-task file.

- **New `/review-board` skill — the heavyweight, on-demand pre-merge review.** Convenes a
  board of expert hats (correctness, security, PM/UX/maintainer, and any hat the change
  earns) as parallel reviewers, checks the diff against the project's *own* `.agent/rules/`
  and gate — no hardcoded laws — then synthesizes one ranked verdict and an ordered fix plan
  you can queue with `coop tasks add`. Ships into new projects via `coop init`, and is
  agent-agnostic: it falls back to a sequential pass where parallel subagents aren't available.

- **Security hardening (from end-to-end and multi-agent audits).** Secret shadowing now covers
  `*.yaml`/`*.yml` credential files and matches filenames case-insensitively, so
  `config/credentials.yaml`, `.ENV`, and `ID_RSA` no longer slip into the box (notably on
  case-insensitive filesystems); a template/sample suffix (`*.example`/`*.sample`/`*.template`) no
  longer un-shadows a private-key pattern, so `id_rsa.example` stays shadowed — only exact public
  CA-bundle names still override `*.pem`. The fork **merge gate** now reuses that same shadow denylist
  instead of a separate regex that had drifted, so `kubeconfig`, `.npmrc`, `.netrc`,
  `service_account.json`, `*.kdbx`, etc. can no longer land silently on `coop fork merge`. `coop
  check-secrets` (and the merge gate) also flag `secret_key_base`/`master_key`/`encryption_key`, a
  password embedded in a connection-string URL (`postgres://user:pw@host`), and GitHub fine-grained
  tokens (`github_pat_…`), and note how many gitignored-but-box-visible files the default scan
  skipped. `COOP_EGRESS` fails closed — an unrecognized value (a typo like `None`) is treated as
  offline, and the box forces `--network none` for any egress value other than an explicit `open`
  (defense in depth at the boundary). `coop login --profile` rejects a traversal/collision name, so
  credentials can't be written outside the agent vault.

- **Per-fork credential profiles in `.agent/fleet`.** Add `profile=<name>` (or `profile=a,b`) to a
  fleet line — e.g. `api codex .agent/tasks.api profile=work` — to put that fork's loop on specific
  account(s); several rotate on a rate limit. Give each fork a different account so a fleet runs in
  parallel instead of all forks contending for the repo pool's first profile. It's a `key=value`
  suffix, so it doesn't add to the fragile space-delimited positional fields, and `coop fleet up`
  validates the profiles up front. Also exposed on `coop fork <name> <agent> --loop --tasks <p>
  --profile a,b` (and a single `--profile` on an interactive fork). Forks with no `profile=` keep
  rotating the repo pool / all signed-in profiles as before.

- **Pick a credential profile for a single run with `--profile <name>`.** Previously `--profile` only
  worked for `coop login`; on a run it was forwarded to the agent and rejected. Now `coop claude
  --profile work` runs that one session on the `work` profile without changing the default
  (`coop profiles default` still sets the persistent one). It works on every agent-launch path —
  `coop <agent>`, `coop fusion <agent>`, and `coop acp <agent>` (so an editor can pin two ACP entries
  to different accounts). coop consumes the flag only before a `--`, so an agent's own `--profile` is
  still reachable as `coop codex -- --profile <name>`; a nonexistent profile errors instead of
  creating an empty husk dir.

- **`coop profiles` flags an expired login.** "signed in" used to mean only that a credentials file
  existed, so an expired OAuth token read as fine yet 401'd mid-run. `coop profiles` now detects the
  expired token and points you at `coop login <agent> --profile <name>` to refresh it.

- **Delete a stored profile with `coop profiles rm <agent> <profile>`.** Removes that profile's login
  token and session history. It refuses to delete the marked default (set another first) and never
  touches the legacy flat layout's whole agent dir. Use it to clear a stray profile left behind by an
  earlier login layout, e.g. `coop profiles rm claude default`.

- **Remote (HTTP/SSE) MCP servers in `mcp.json` are covered for every agent.** Besides stdio
  servers, an HTTP server — `{ "type": "http", "url": …, "headers": … }` — reaches all three:
  claude and gemini read the canonical `headers` directly, and codex (which has no inline-header
  support — only `bearer_token_env_var` / OAuth) authenticates via `bearer_token_env_var`, so set
  both on a server you want all three to use. coop now flags a codex HTTP server that carries
  `headers` but no `bearer_token_env_var`, so the auth gap is visible instead of a silent 401. See
  `agents/mcp.json.example`.

- **Reliability fixes (from end-to-end and multi-agent audits).** Concurrent `coop pool add` /
  `coop profiles default` no longer lose updates (writes are locked + atomic), and `coop fork -d`
  claims its pidfile atomically (the worker owns it on exit) so two concurrent detaches can't start
  two loops racing one worktree; `coop fork stop` confirms the worker is dead (escalating to SIGKILL)
  before clearing the pidfile. `coop fork merge` rebases the fork's branch by name, so it can't
  sign/land the wrong branch if an agent left another checked out. `coop fork --profile <typo>` fails
  before cloning (no stray fork), a fork can't be named `acp`, and a duplicate fork name with
  whitespace/`=`, a duplicate profile in `--profile a,a`, and a duplicate task id across states are
  all rejected/de-duped. Regenerating a Codex MCP config no longer drops a user's own
  `[mcp_servers_backup]`-style tables, and numeric MCP env values render as plain digits. The
  unattended loop no longer false-"stall"s when it's parking one-way-door tasks into `50_blocked/`
  (progress = done *or* blocked), parses minute/hour `retry-after` hints, and falls back to backoff
  on an unrecognized reset timezone instead of waking hours early; `coop tasks watch`/`fleet watch`
  counts no longer briefly inflate on a torn read of a task move. Fusion's governor is told to consult
  only authenticated peers.

- **CLI consistency.** `coop version`, `coop loop`, `coop acp`, and `coop pool` reject stray or
  malformed arguments instead of silently ignoring them; `coop fork merge` with no name reports a usage
  error; `coop help help`/`coop help version` no longer print a broken pointer; `--consult`/`--supervise`
  honor the `--` separator; `coop fleet down` surfaces a running fork no longer in the fleet; and
  `coop profiles` flags a dangling default.

- **`coop update` now refreshes agent CLI packages from npm's stable `latest` tags.** The shared
  box's built-in npm specs are `@anthropic-ai/claude-code@latest`, `@openai/codex@latest`,
  `@google/gemini-cli@latest`, `@agentclientprotocol/claude-agent-acp@latest`, and
  `@agentclientprotocol/codex-acp@latest`, so a fresh `coop update` picks up agent fixes without
  a coop source change. Codex profiles are also hardened before launch with a best-effort SQLite
  trigger that ignores inserts into `logs_2.sqlite`'s feedback-log table (openai/codex#28224);
  sessions, auth, MCP config, and memories are left alone.

- **`COOP_NO_ASDF=1` no longer breaks Node-based agent CLIs when the shared asdf volume has a
  stale Node shim.** The flag still skips `.tool-versions` provisioning, but the entrypoint now
  always repairs a broken bare `node` by selecting an installed asdf Node fallback when needed.

- **Readable, colorized output across the CLI.** `coop tasks` and `coop tasks decisions` render in
  color — colored state headers, wrapped titles, a gray task id, and `[n/m]` subtask / `⚠` decision
  markers — instead of a flat monochrome list, degrading cleanly on a narrow terminal or under
  `NO_COLOR`. Command results now speak in one voice: a green `✓` for success, a yellow `⚠` for a
  caution, a red `✗` for a failure, plain text for a neutral note — replacing the old
  `coop: <command>: …` prefix that just echoed the command back at you (so `coop tasks lint` reads
  `✓ no issues — 1 task checked`). The loop's per-iteration line reads `· using <agent> model
  <model> profile <profile>` with the values lifted out of the dim label, and `coop fleet watch`
  repaints only on a real change (no flicker), with the task counts aligned in a right-sized column.

- **The unattended loop's live view no longer garbles.** The bottom status bar was overprinted by
  coop's own `coop:` lines and the sibling-services startup; those now scroll cleanly above the bar
  (ui routes them through it), the auto-start no longer dumps docker compose's repainting progress
  (it's discarded — `coop up` shows it), and the streamed Bash activity shows the real command
  instead of the `cd …` an agent prefixes to reach a monorepo subdir. The bar also skips a repaint
  when nothing changed (same content + width), like `coop fleet watch`, so it sits still instead of
  flickering. The loop prompt also reminds the agent to read a file before editing it.

- **Shared guidance for using agent orchestration well.** The repo contract, `coop init` scaffold,
  and global `INSTRUCTIONS.md.example` now teach every supported agent to set a persistent goal when
  its runtime has one, batch independent read-only work, and use native subagents or task workers for
  research and second opinions while keeping writes serialized in the current checkout. The wording is
  capability-based so Claude, Codex, and Gemini use their native equivalents without inventing
  unavailable slash commands or trying to run host-side Coop commands from inside the box.

- **`rm` is the one verb for deleting things.** Every destructive subcommand advertises `rm` —
  `coop tasks rm`, `coop fork rm`, `coop profiles rm`, `coop pool rm` — rather than `coop tasks`
  alone advertising `remove`. `remove` still works everywhere as an accepted alias, so existing
  habits and scripts don't break; it's just no longer the form shown in help or usage.

- **Clearer `coop help`.** Tasks get their own `TASKS` section instead of one line under
  `UNATTENDED`, and `coop up`/`coop down` name the actual services defined in `compose.agent.yml`,
  dimmed (with a "none yet" hint) when there's no compose file to act on.

- **A bare command group prints its help instead of an error.** `coop tasks` and `coop fleet`
  with no subcommand used to fail with an "unknown command" error built from an empty token; they
  now print that group's help and exit 0 — the natural way to discover the subcommands. (`coop
  pool` and `coop profiles` already showed a sensible default view, so they're unchanged.)

- **Task-queue niceties.** `coop tasks --tasks <dir> add` bootstraps a missing secondary queue on
  demand, so a monorepo can start a per-component queue without a root `coop init`. (The single-loop
  progress view — done/total · blocked · the active task — is now part of `coop tasks watch`.)


- **`coop tasks` works across every configured queue.** With several queues (a monorepo's
  `COOP_TASKS`, or repeated `--tasks`), only `ls` and the non-interactive `decisions` used to roll
  up — everything else bailed with "works one queue at a time". Now `decisions -i` walks every
  queue's open decisions in one interactive session (each header names the queue), `lint` rolls up
  with the worst exit code, `rm --all-done` clears every archive, and the id-addressed commands
  (`claim`/`block`/`unblock`/`done`/`rm <id>`) find their task in whichever queue holds it — same
  exact-then-substring matching as before, erroring only when an id matches in more than one queue
  (`split` slices share ids with their source, so acting on an arbitrary copy would touch the wrong
  tree). Only `add` and `split` still need a single `--tasks`, since they create into a queue.

- **The orchestrator pattern, scaffolded and documented.** `coop init` now writes two starter
  subagents alongside the skills: `.claude/agents/deep-reasoner.md` (pinned to Opus — reasoning-heavy
  phases: architecture, complex debugging, algorithm design) and `.claude/agents/fast-worker.md`
  (pinned to Sonnet — mechanical, fully-specified work). They're native Claude Code subagents whose
  descriptions route delegation automatically, so a lead running on a bigger model (`coop claude
  --model claude-fable-5 --consult`) spends its own tokens on planning and synthesis while each
  delegated turn bills at the subagent's cheaper model. The README's new "orchestrator pattern"
  recipe ties it together with the existing pieces: per-profile model marks for the lead, and
  codex/gemini as read-only peer engineers via `coop-consult` under `--consult`/fusion — including
  the gotcha that a plain run deliberately doesn't mount peer credentials, so peers only answer in
  a consult/fusion box. Existing files are never clobbered; edit the subagents freely.

  And the pattern now runs unattended: **`coop loop --consult`** (and
  `coop fork <name> <agent> --loop --consult`) opts every iteration into peer consultation — the
  box mounts the authed peers' credentials and the `coop-consult` wrapper, so a headless lead can
  get second opinions on hard calls. A fleet opts in per fork with **`consult=1`** on its
  `.agent/fleet` line. Off by default everywhere, since it widens each iteration's credential
  scope to the authed peers.

- **Pick the model for any run — `--model` everywhere, per-profile defaults, and `coop models`.**
  Every launch path now takes `--model <m>`: `coop claude --model opus`, `coop fusion claude
  --model fable`, `coop loop --model haiku`, `coop fork risky claude --model opus`, `coop acp
  claude --model sonnet`, and a fleet line's `model=` option. For standing defaults, mark a model
  per credential profile with `coop profiles claude work model opus` (`--clear` unmarks; the mark
  is a profile attribute, so `coop profiles` owns editing it and shows it) — e.g. the work
  subscription always on the big model, the personal one on a cheap one; the loop's profile
  rotation then switches models with the account. `COOP_<AGENT>_MODEL` sets an agent-wide default
  and `COOP_LOOP_MODEL` a loop-only one (so overnight runs grind on a cheaper model than your
  interactive sessions). `coop models [agent]` is the model menu — one line per agent with its
  known models (examples — any id the CLI accepts works; coop never validates one) and a short
  how-to; each profile's mark shows as a column in `coop profiles`.

- **`coop profiles` gained a path grammar** — each token narrows: `coop profiles` lists every
  agent, `coop profiles claude` one agent, `coop profiles claude personal` shows that profile,
  and a trailing attribute edits one property of it: `model [<m> | --clear]`, `default` (mark it
  what a plain `coop claude` runs), `rm`. So profile edits read as a path — `coop profiles
  claude personal model opus-4.8` — instead of a verb sandwich (`coop profiles default claude
  default` read like a stutter). The old verb-first forms (`coop profiles default|rm <agent>
  <profile>`) still work.

  *How it reaches every feature.* The precedence is most-specific-first: `--model` ›
  `COOP_LOOP_MODEL` (loop runs) › the profile's mark › `COOP_<AGENT>_MODEL` › a model baked into
  `COOP_<AGENT>_CMD` › the agent CLI's own default. The resolved model rides each agent's command
  as its native `--model` flag (interactive, headless loop iterations, fork session resume, the
  fusion governor, gemini's ACP), is exported as the agent's own model env var for flagless
  adapter binaries (claude-agent-acp reads `ANTHROPIC_MODEL`), and reaches fusion/consult *peers*
  via `COOP_PEER_MODEL_<AGENT>` vars the `coop-consult` wrapper expands — so each peer answers on its
  own configured model. One gap, by design: codex under ACP keeps reading its model from its own
  `config.toml` (its adapter takes no flags and codex has no model env var).

- **Fixed: a deleted profile named "default" kept reappearing (empty, "not signed in").**
  Every box run pre-created the active-profile dir for ALL THREE agents — even agents the run
  never touched — and with no profile marked default (or via a bare `coop login <agent>`, which
  targeted a profile literally named "default"), that materialized a husk `default` dir seeded
  with settings files, resurrecting it after every deletion. Three changes close it: a run now
  pre-creates homes only for the agents it actually mounts (the launched agent plus authed
  fusion/consult peers); a bare `coop login <agent>` signs into the agent's MARKED default
  profile — the one your runs actually use, so re-authing an expired subscription lands in the
  right slot instead of a fresh "default"; and a custom-`COOP_LOOP_CMD` loop pins the marked
  default too.

- **Fixed: a fork (or `coop <agent> --consult`) with only one agent signed in launched with no
  instructions at all.** The lead agent's global instruction file — its `CLAUDE.md` / `AGENTS.md` /
  `GEMINI.md`, carrying the box environment briefing and your shared `INSTRUCTIONS.md` — was
  silently not mounted whenever the lead had no authenticated peer to consult. Since `coop fork`
  always runs the agent as a consult lead, a single-agent `coop fork <name>` gave the agent none of
  that context (it would rediscover the box the hard way, e.g. investigating the expected missing
  bubblewrap). The lead now always receives its base instructions; the read-only consult directive
  is still added only when a peer is actually signed in.

- **Fixed: an unrelated number containing `429` could stall the loop as if rate limited.** The
  loop's rate-limit detector matched the bare substring `429`, so a failed iteration whose output
  happened to contain e.g. `1429 files` was treated as an HTTP 429 and made the loop wait. It now
  matches the status only as a standalone `429`.

- **A foreground `coop fork <name> --loop` now gets the same Ctrl-C soft stop as `coop loop`.**
  The graceful-stop handler was gated to exclude every fork, though its rationale only covered
  *detached* workers (which have no terminal); a foreground fork loop was hard-killed mid-iteration.
  It's now keyed on owning a terminal, so a foreground fork loop finishes the current task then
  stops, while the detached worker (stdin is `/dev/null`) is still left to `coop fork stop`.

- **`coop loop` stops gracefully on Ctrl-C — finish the current task, then stop.** A first Ctrl-C
  on a foreground loop is now a *soft* stop: the running iteration finishes and commits, then the
  loop stops before claiming the next task, instead of the box being killed mid-task. Press Ctrl-C
  again to stop now — the running box is torn down cleanly (SIGTERM then SIGKILL of its whole
  process group, so no container is orphaned). The rate-limit and retry waits wake on the stop too,
  so Ctrl-C stays responsive even during a long wait.

  *Why it's better.* The old Ctrl-C was a hard kill — it tore the box down mid-iteration, losing
  the in-flight turn's uncommitted work. The soft stop lets the agent reach a clean, committed
  checkpoint before the loop exits. It works by running each iteration's box in its own process
  group, so the terminal's Ctrl-C reaches only coop, which then decides whether to let the box
  finish (first press) or tear it down (second). Detached fork loops are unaffected — they have no
  terminal and are still stopped with `coop fork stop`.

- **Loop activity shows tool paths repo-relative, and flags anything outside the repo.** In the
  live `coop loop` view, a file tool's path now renders relative to the repo root —
  `✎ Edit internal/cli/streamjson.go` instead of the full container mount path. When the agent
  reads or writes a path *outside* the repo tree, the line keeps the whole path and is flagged
  with a yellow `⚠`, so an escape from the working tree stands out at a glance. The root is the
  repo's real in-box mount (`box.Workdir`), so the inside/outside call matches where the repo
  actually lives in the box — and a shared string prefix (`…/proj-x` vs `…/proj`) is never
  mistaken for containment.

## 2.10.1

- **The loop continues an interrupted task instead of stranding it `[w]`.** When an iteration was
  killed mid-task — a rate limit, crash, or timeout after the agent claimed `[w]` but before it
  committed — the claim used to sit `[w]` forever: later iterations only picked up `[ ]` items, and if
  the stuck task was the last one the loop exited reporting "✓ done". Now the loop keeps running while
  any `[ ]` **or** `[w]` remains, and the work prompt tells the agent a `[w]` is an interrupted attempt
  whose partial work may be uncommitted — to inspect `git status`/`git diff` and continue it (or
  discard and redo if it's off-track) before starting new `[ ]` work. A stall guard stops the run if
  no task completes for several iterations, so a genuinely unfinishable task can't spin forever. This
  pairs with the loop's existing profile rotation (`coop pool`): a rate limit switches subscriptions
  and the new profile continues the same task from its on-disk partial work.

## 2.10.0

- **Every box auto-starts the repo's sibling services (`compose.agent.yml`).** Previously only the
  explicit `coop up` brought services up; a box merely *joined* their network if it already existed,
  so launching `coop fusion` / `coop acp` / `coop loop` / a fork without remembering `coop up` first
  left the agent unable to reach its db/redis. Now every launch path funnels through `box.Run`, which
  runs `compose up -d --wait` before starting the box whenever a compose file is present — so services
  are up and healthy for any mode. It's idempotent (already-running services are a fast no-op), gated
  the same way as the network join (skipped offline via `COOP_EGRESS=none`, on the Apple `container`
  runtime which has no compose, or with `COOP_NETWORK=0`), and never blocks the session — a failure
  warns and continues. Opt out with `COOP_AUTO_UP=0` to keep managing services with `coop up`/`coop down`.

## 2.9.0

- **A `claude` peer consult no longer hangs forever (`coop-consult` detaches peer stdin).** `claude -p`
  reads piped stdin *in addition to* its prompt argument, so when the lead backgrounded a consult the
  peer inherited an open pipe and blocked reading it until killed — a `claude` consult timed out at the
  status line while `gemini` returned. The wrapper now captures the prompt up front and detaches stdin
  (`exec </dev/null`) for every peer, so none can block on it (codex's per-call redirect folds into the
  one shared redirect).

- **Each `coop-consult` is time-bounded so a slow or wedged peer can't stall the lead.** Every peer
  call now runs under a timeout (default **30 minutes**); on expiry the peer is skipped with a clear
  notice so the lead synthesizes from whoever answered, instead of the consult blocking and the lead
  hand-rolling its own `timeout`. Set `COOP_CONSULT_TIMEOUT` (seconds) — via env or `coop.conf`, now
  forwarded into the box — to tune it per run.

## 2.8.0

- **`coop loop --preflight` runs a cleanup pass before working the queue.** Opt-in (`--preflight`, or
  `COOP_PREFLIGHT=1` to default it on; `--no-preflight` overrides). Before the first work iteration it
  runs one agent pass that compacts `.agent/LOG.md`, removes done `[x]` tasks already committed, and
  unblocks any `[B]` item whose `.agent/PENDING_DECISIONS.md` entry now has an answer — so a fresh run
  starts from a tidy queue. It works no task and makes no commits (these files are git-ignored); it's
  the symmetric front bookend to the existing audit pass, and is skipped under `COOP_LOOP_CMD`.

- **The box repairs a bare `node` when an orphaned asdf nodejs shim shadows the image's node.** After
  you run coop on a repo that pins `nodejs` in `.tool-versions`, asdf keeps that node shim in the
  persisted `~/.asdf` volume. In a later repo that doesn't pin nodejs (e.g. a Go project), the shim
  shadowed the image's node and failed with "No version is set for command node" — which broke the
  Node-based agent CLIs invoked as subprocesses, so `coop fusion` / `--consult` peers (and any node
  tool an agent shelled out to) errored even though the lead agent itself ran. The box entrypoint now
  detects a broken `node` and pins the newest installed asdf nodejs as the global fallback; a repo's
  own `.tool-versions` still overrides it. Run `coop build` to pick it up.

- **Fusion/`--consult` peers can keep their thread across turns (`coop-consult` wrapper).** The
  governor/lead now consult peers through a small mounted `coop-consult <peer> --fresh|--continue`
  script instead of raw `claude -p …`/`codex exec …`/`gemini -p …`. `--fresh` starts a new read-only
  session; `--continue` resumes the peer's *own* prior consult, so a follow-up sends only the delta
  instead of re-pasting the static context. It hides the per-agent session-id mechanics (claude/gemini
  start under a generated `--session-id`, codex's `thread_id` is captured from `exec --json`) and
  prints whether it continued or fell back to fresh, so the lead knows when to resend full context.
  `--consult` defaults to `--fresh` (independent second opinions); fusion's instruction tells the
  governor to continue within a subject and start fresh on a new one. Peers stay read-only throughout.

- **Fork re-entry resumes the right session, even after a loop or consult ran in the fork.** Re-entry
  used to resume the *most-recent* session for the fork's directory (claude `--continue`, gemini
  `--resume latest`), which a `coop fork … --loop` iteration or a fusion/`--consult` peer call sharing
  that directory could win — landing you in the wrong thread. coop now assigns claude/gemini forks
  their own session id (stored in the fork's git-excluded `.coop/`) and resumes exactly it; codex,
  which can't be handed an id, resumes the most-recent *interactive* session and skips `codex exec`
  loop/consult runs. This also sidesteps gemini's basename-keyed session store (same-named forks in
  different repos no longer collide on resume).

- **Fusion governor consults peers with self-contained prompts, not your message verbatim.** The
  governor is now told that peers are read-only advisors — it consults them on the *thinking* a task
  needs and makes every change itself — and that each consult is a fresh, memoryless call, so it
  composes a self-contained prompt (goal + context + question) rather than forwarding your message
  verbatim, which was meaningless to a peer past the first turn (e.g. "fix the second one").

- **`coop update` now self-updates the `coop` binary, then rebuilds the box image.** Previously it
  only refreshed the box (base image + agent CLIs + ACP adapters), leaving the binary pinned at
  whatever you installed. It now first upgrades `coop` to the latest GitHub release — fetched and
  verified the same way `install.sh` does (checksum + cosign), and swapped in atomically (rename, not
  in-place write) so replacing the running binary is safe — then rebuilds the image. `--self-only`
  updates just the binary; `--box-only` keeps the old box-only behavior. A dev/source build, an
  already-current binary, or a coop installed somewhere unwritable (a package-manager prefix) skips
  the self-update with a note; an offline check is a soft warning that still lets the box rebuild.

## 2.7.2

- **A crashed fork whose PID gets reused is no longer mistaken for "still running".** Liveness only
  checked that the recorded PID existed, so after a worker crashed and the OS reused its PID for an
  unrelated process, the fork read as running — blocking `coop fleet up` / `coop fork merge` /
  `coop fleet prune` on it. The pidfile now records the worker's start time, and a PID whose start
  time no longer matches is treated as dead (a missing/uncheckable start time stays "alive", so a
  genuinely live loop is never wrongly double-started). Old pid-only pidfiles still work.

## 2.7.1

- **`coop fleet up` is loud when it aborts partway.** It still fails fast on the first fork that
  can't start (a silent partial fleet discovered hours later is worse), but the error now says how
  many forks already started and how to clean them up (`coop fleet down`), instead of only naming the
  fork that failed.

- **`coop fleet watch` and the loop progress bar ride out a torn `TASKS.md` read.** The agent
  rewrites its queue as it works; a refresh landing mid-rewrite could read the file empty for an
  instant and flash a wrong "0/0" / "(no queue)". A populated queue never legitimately empties, so a
  zero-task read now keeps the last good counts.

- **`coop fork merge` refuses to land a fork whose loop is still running.** Merging rebases inside
  the fork's worktree and then deletes it; doing that to a fork whose detached loop is mid-iteration
  corrupted the in-flight work and orphaned the worker. `coop fork merge <name>` now errors if that
  fork is still running (stop it first), and `coop fork merge --all` skips still-running forks with a
  notice and lands the rest — matching how `coop fleet prune` already guards.

- **`.agent/fleet` rejects a misspelled agent, a path with spaces, and duplicate fork names.** A line
  like `api borg .agent/TASKS.api.md` silently treated `borg` as the path (dropping the real one),
  surfacing later as a baffling "no such file: borg"; a path with spaces was truncated to its first
  word; and a repeated fork name was accepted, silently dropping the second. These are now parse-time
  errors that name the problem.

- **The loop names the task queue (and `AGENTS.md`) as absolute paths, so Gemini/Codex fleet forks
  can read it.** Each iteration told the agent to `Read .agent/TASKS.md …` — a repo-relative path.
  Claude/Codex resolve it against the box's working dir, but Gemini's `read_file` rejects a relative
  path outright, so a Gemini (or Codex) fork in a fleet couldn't read its own queue and stalled at
  0/N. The prompt now names absolute in-box paths.

- **`coop fleet watch`: a fork with only blocked tasks left reads as "blocked", not "✓ done".** The
  row used "no actionable task" as the done signal, but `scanTasks` reports that for an all-`[B]`
  queue exactly as for an all-done one — so a fork at 2/5 with 3 blocked (even while still running)
  flashed "✓ done". "✓ done" now requires every task to be `[x]`; an unfinished fork with nothing
  actionable shows "blocked".

- **`coop fleet watch` shows a stopped fork as "stopped", not "paused".** A fork whose loop had
  exited with tasks still unchecked was painted with the idle `‖` glyph and its *next* task name, so
  a fork that quit at 0/20 read as "paused, working on Task 1". Such a fork now shows a yellow mark
  and "stopped" and stays legible; only a fork that never started (no log) recedes. (Done forks are
  still `✓ done`, running forks still spin.)

- **`coop fleet watch` no longer spams its header into scrollback.** The live dashboard repainted a
  bottom-pinned region, which redraws in place by counting lines up from the bottom — but once the
  dashboard was taller than the terminal pane, every refresh scrolled the top line (`coop fleet — N
  running…`) into scrollback instead of overwriting it, leaving a growing wall of repeated headers.
  It now renders on the alternate screen buffer (like `top`/`htop`): each frame repaints from the
  top-left and the prior screen is restored on exit, so it never pollutes scrollback regardless of
  window size.

## 2.7.0

- **`.coopignore` can re-hide a whitelisted template / CA-bundle name.** AllowGlobs (`*.example`,
  `*.sample`, `*.template`, `cacerts.pem` and friends) used to win over *both* the built-in secret
  patterns and an explicit `.coopignore`, so a name like `cacerts.pem` or `.env.example` stayed
  visible even when you listed it in `.coopignore`. AllowGlobs now overrides only the built-in
  false positives; an explicit `.coopignore` entry is authoritative and re-hides the file. Defaults
  are unchanged — templates and public CA bundles still stay visible unless you opt to hide one.

- **Value-bearing CLI flags reject a missing value or stray argument instead of silently doing the
  wrong thing.** `coop login claude --profile` (no name) used to fall back to the default profile;
  `coop login claude extra`, `coop init --bogus`, and a trailing `coop tasks --tasks` were silently
  ignored. They now exit 2 with a message naming the missing value or unknown flag. A shared
  `flagValue` parser handles `--flag value` and `--flag=value` for `--profile`, `--stack`,
  `--services`, and `--tasks`; agent pass-through flags are unaffected.

- **`coop acp --supervise` teardown and replay are hardened.** Three narrow lifecycle bugs in the
  supervise/restart path are fixed: (1) tearing down during the startup window (after `docker run`
  begins but before the box is labelled) could orphan the container — the inner `coop acp` now runs
  in its own process group (killed as a group, so its `docker run` grandchild dies too) and the box
  gets a deterministic `--cidfile`, so the supervisor removes it by id even before labels exist;
  (2) replay no longer blocks forever on a hung restarted agent — it waits generously for the first
  response (a cold box may be provisioning) then bounds the gap between responses, failing in-flight
  requests and giving up cleanly instead of freezing the editor; (3) a `session/new` in flight when
  the child died no longer leaks its pending-request entry. `--supervise` is unchanged for users.

- **Task-queue contract now matches the parser: every top-level `[ ]` is live.** The docs said
  the loop works `[ ]` items "under `## Active`", but the loop/split/status act on every
  unchecked top-level task wherever it sits. AGENTS.md, the scaffolded `coop init` templates,
  README, and `coop tasks lint`'s wording now state the real rule — `## Active` is convention,
  the example uses `[E]`, and anything not ready to work belongs in `BACKLOG.md`/`IDEAS.md`.

- **`install.sh` fails closed when `checksums.txt` lacks the selected asset.** Previously, if
  the checksum file was fetched but had no line for `coop_<ver>_<os>_<arch>.tar.gz`, the
  extracted checksum was empty and the mismatch check was skipped — an unverified binary
  installed silently. A missing entry is now treated as a release-integrity failure and aborts.
  The verification is factored into a `verify_checksum` helper (unit-tested without network);
  cosign signature verification is unchanged.

- **`coop check-secrets` is honest about gitignored files, and `--include-ignored` scans them.**
  The default scan covers the commit-candidate files (tracked + untracked, gitignored excluded)
  — but a `coop run`/`shell`/`loop` bind-mounts the *whole* working tree, so a
  gitignored-but-not-shadowed file (a stray `serviceAccount.json`, say) is still visible to the
  agent even though the old docs implied the scan saw "everything not shadowed". The output now
  names which scope ran, and `coop check-secrets --include-ignored` walks the full visible tree
  (still skipping shadowed files, dependency/build dirs, and binaries). README + help updated.

- **A run now mounts only the launched agent's credentials, not every agent's.** A plain
  `coop claude` used to bind-mount `~/.claude`, `~/.codex`, *and* `~/.gemini` (and pass the
  whole `agents/env` with every API key) into the box, so the Claude process could read the
  Codex/Gemini logins it had no need for. Each run is now scoped: a plain agent run mounts just
  its own home and API key; `coop fusion` and `coop <agent> --consult` (and forks) also mount
  the *authenticated* peers they're told to consult; raw `coop run`/`coop shell`, the merge
  gate, and `coop doctor` mount no agent credentials; `coop login <agent>` mounts only the agent
  being signed in. Peer API keys are filtered out of the env file for scoped runs; non-key
  runtime vars still reach every box. `COOP_HOMES=0` still disables homes entirely.

- **`.tool-versions` toolchains (go, ruby, …) now resolve in a login shell too.** The box puts
  asdf's shims on PATH via the image `ENV`, which only reaches the agent process and non-login
  shells; a login shell (`sh -lc`, `bash -l`) sources `/etc/profile`, which resets PATH to the
  Debian default and dropped the shims — so a gate run through a profile-sourcing shell saw
  `go: not found` even though go was installed and shimmed (node/python/git survived, living in
  `/usr/local/bin`). The base box now ships an `/etc/profile.d/asdf.sh` drop-in that re-adds the
  shims for login shells. Rebuild with `coop build` to pick it up.

- **`coop acp --supervise` no longer drops a request when the box restarts.** On a restart the
  proxy failed the in-flight requests (telling the editor to retry) a beat *before* it repointed
  to the new child, so a retry that arrived in that window was written to the dead child and
  silently dropped (a tight timing race; it also surfaced as an intermittently deadlocking test).
  The new child is now published before the retry signal, so the retry lands on it.

- **`coop loop` shows which model each iteration is running.** When Claude streams its activity on a
  TTY, the loop now surfaces the model from the agent's `init` event (e.g. `· model claude-opus-4-8`)
  right after the iteration banner — so a long unattended run shows the model actually working,
  including when credential-pool failover rotates to a different account or tier. coop doesn't choose
  the model, so this reflects the agent's own report; non-streaming agents and detached fork logs are
  unchanged.

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
