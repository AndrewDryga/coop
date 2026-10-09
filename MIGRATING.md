# Migrating

## v11.0.0: upgrading from v10.1.2

Coding and editor sessions now use complete native homes scoped to their repository. Ordinary
homes are also scoped to the selected account; editor homes survive account switches. Native
settings are seeded once and then remain local. Validated forks share the parent repository domain.

On first reuse, co:op imports only older transcripts with proven repository ownership. Originals
stay intact. Ambiguous or unsupported history is retained, not exposed to another project; active
writers defer import rather than being stopped. Finish those sessions and retry. Native deletions
are recorded by import receipts, so an old source cannot silently resurrect deleted sessions.

Account credentials move into one canonical host-only authority. Old credential sources are
retired only after recoverable publication and writer checks; no refresh grants are copied into
repository homes. The host renews grants while a run-local broker serves native provider requests.
Online provider runs now require Docker, including open-network runs. Raw uncredentialed workloads
can still use Apple's container runtime.

Use host `coop login <provider>@<account>` for sign-in and the confirming
`coop credentials <provider> <account> rm` flow for removal. Removal deletes that account's ordinary
repository histories, but not account-independent editor conversations. Native in-box login/logout
is not a supported way to manage the shared account. If it changes local credential files, co:op
refuses reuse and names both the native home and its recovery custody directory. Stop sessions
using that home and back up the entire custody directory, including `home/`, `owner.json`, auth
and import receipts. Recover locally created credentials through an explicit host sign-in. Restore
public auth files from a known-good backup, or explicitly move the entire backed-up custody directory
aside to start fresh. Moving only `home/` leaves ownership receipts behind and will be refused.
Keep the backup: a fresh home is not proof that its unique history has been recovered.

Before upgrading an existing installation, finish older sessions and keep a verified backup of
its stopped credential/history roots. After credential cutover, do not run older binaries against
those roots. A rollback must use current authority or a fresh host sign-in; restoring pre-refresh
tokens can invalidate the remaining account access. Arbitrary old binaries and native host clients
do not participate in co:op's writer fencing.

## v10.1.2: upgrading from v8.1.0

This section covers the changes since v8.1.0. Do the steps below that apply to you before you use
the new binary to start workers, fork loops or sessions with MCP.

### Controller-owned workers and session schema 25

A worker now connects with one command:

```sh
coop sessions connect --controller https://controller.example --token-file /private/enrollment-token
```

Before you run it against an existing root, prepare that root:

1. Stop the connection or service that owns it.
2. Back it up. Use the old binary's `sessions compact --backup <path>` when it's available, or a
   verified copy of the stopped root.

When the new service opens the state, it upgrades the SQLite schema to 25. An older binary can't
reopen the upgraded root, so keep the backup and the old binary for rollback.

Use `--state <path>` for a separate private worker root, and `--ca-file <path>` for a private
controller CA. The connection owns its local service. It refuses a root that is already listening,
and never reuses or replaces it. `coop worker`, `sessions serve`, `sessions policies` and
`connect --config` are gone. There is no worker JSON file or local execution-policy catalog to
translate.

Controllers must send worker protocol v2 and canonical version-2 JobSpec documents, and select
workers that advertise live `job-setup:2`. Each job freezes its setup: source and companions,
targets, mode and networking. It also freezes explicit `environment`, `check.argv` and
`check.environment` values, and positive CPU, memory and PID caps. Work and review use the same
frozen setup, and check-only environment values override work values. Resolve repository defaults before you submit a job:
worker `COOP_GATE` and repository `gate:` no longer choose controller reviews.
[The session API](docs/session-api.md#sessions) lists the fields, limits and digest requirements.

You can still read historical jobs. A job without the current setup can't run new turns, reviews
or interrupted creates. For each of those jobs:

1. Preserve the work and history you need.
2. Where you can, finish, close or discard it through the supported old workflow.
3. Resubmit unfinished work as a new v2 job.

Never edit saved documents or synthesize authority to make an old record run.

Sign in to the provider accounts on each worker with co:op's normal login flow. Controllers don't
transfer model credentials.

The sections below cover the other configuration, runtime and task changes.

### Anchored fork and network identity

Linux overlay filesystems can reuse every observed piece of directory metadata, including the
inode and birth time, right after a deletion. So co:op no longer treats allocator metadata as
durable authority for the two roots that authorize future sandbox work. Fork generations and
project network approvals now use a two-link filesystem anchor.

A fork carries `.coop-fork-generation`, linked to its generation-specific file under the adjacent
owner-only fork state. A reviewed project carries `.coop-network-approval`, linked to a randomly
named file under co:op's owner-only network state. The visible files hold identifiers and no
credentials. Whenever co:op uses the authority, both names must still be the same owner-controlled
`0600` inode with exactly two links.

The two names must be on one filesystem that supports hardlinks. Fork state sits next to its
workspace, so for forks this is normally automatic. For network approvals, keep the checkout and
`$XDG_STATE_HOME` (or the default `~/.local/state`) on the same filesystem. If co:op can't create
the link, it reports a clear error and doesn't fall back to timestamps.

Stopped v1/v2 fork records migrate to hardlink anchors under the lifecycle lock, but only when
their Git branch and absolute local origin still match and they have no execution or land intent.
An ordinary fork must also have no reservation. A remote session may keep only its exact
same-generation, same-owner reservation during the migration. co:op never guesses a missing or
foreign reservation, or live execution, into ownership. If a legacy fork is still active, stop it
and retry.

Older network approvals grant nothing under the new binary. Run `coop approve` once to review and
enroll the current project. Task records and remote-session discard plans keep their format. They
still combine logical identity, semantic state, locks and live pinned handles, instead of relying
on another timestamp.

Ordinary Git status leaves the markers out, but a manual `git clean -x` can still remove them. To
repair a damaged project approval:

1. Inspect it with `ls -l -- ./.coop-network-approval`.
2. Once you've verified that the damaged name is stale, run `rm -- ./.coop-network-approval`.
3. Run `coop approve`.

If you move an intact checkout to a new path, run only `coop approve`. co:op proves and retires the
old pair before it enrolls the new path. Copying the marker's bytes never transfers access, and
never retires the original project's anchor.

To repair a damaged fork marker:

1. Preserve any Git work, task notes or other data you need.
2. Remove the fork and its authority with the normal, confirming `coop fork rm <name> --force`
   flow. This permanently deletes the fork's unmerged work and service volumes, so inspect the
   deletion preview before you confirm.
3. Recreate the fork.

Don't remove only `.coop-fork-generation`. The generation record and the private link must retire
together. co:op's own checkpoint restore preserves an intact marker.

When an anchor exists, runtime arguments may mount the exact project. They may not mount a path
inside it that an existing agent could swap before the runtime resolves the bind, or a writable
ancestor that could replace the project. No mount may expose co:op's private anchor,
execution-record or launch-lock state, even read-only, and that includes paths the first approval
or fork will create. co:op refuses opaque `--volumes-from` and custom volume drivers, and inspects
existing named volumes before launch. Put a cache outside the checkout and mount that explicit,
inspectable path instead.

Saved service-file approvals from earlier formats, including the first repository-scoped format,
no longer grant access. Run `coop up` at a terminal to review eligible read-only secret files
again. You can't approve secret directories or writable binds.

An external or custom-named Docker volume now also needs an explicit terminal review. The review
lists the volume's actual name, its read-only/read-write mode and its service targets. Automatic,
filtered and non-terminal starts stay closed, even after approval. The grant binds the selected
Docker daemon and the inspected plain-local volume object, so switching the daemon or replacing a
volume needs another review.

The approval belongs to the repository's `.coop-service-approval` marker and a private hardlink in
co:op state. Keep both on one filesystem. Copying the marker into another checkout doesn't copy its
grant. Inspect a damaged marker before you remove it and review again.

co:op now refuses sidecar repository binds that use SELinux relabel or propagation options. Remove
those options before you retry. A read-only session also requires its bind source to exist
already, because Compose's short syntax would otherwise create a directory on the host.

### Canonical tasks across isolated forks

Fork loops now schedule from the project's canonical task queue. They no longer copy a complete
`.agent/tasks` tree into each fork, and `coop tasks split` is retired.

Before you upgrade:

1. Stop every detached fork loop with the co:op version that started it.
2. Inspect each fork's Git work and copied task folders.
3. Preserve any fork-only notes, decisions, artifacts or unfinished changes before you start the
   new loop. The new controller deliberately doesn't choose which of two copied folders is true.

The first new loop on a fork refuses to start while that workspace still has tasks under its own
`.agent/tasks`, including configured subproject queues. Review and reconcile those copies, then
recreate the fork with `coop fork <name> <target> --fresh --loop`. Add `--force` only after you've
reviewed any unmerged Git work and decided to dispose of it.

co:op neither imports nor deletes old `.agent/tasks.sliceN` directories in the canonical checkout.
Turn genuinely distinct work into canonical tasks yourself, and archive the old slice only after
you've verified that every note and change has a home.

After you migrate, start any number of forks against the same queue:

```sh
coop fork perf codex --loop -d
coop fork docs claude --loop -d
coop tasks watch
```

`--tasks <path>` now selects a canonical queue. It's no longer a copy destination. The host gives
each fork one durable assignment, and exposes only that task in `.coop/task-executions/`. The
assignment survives a stop or a crash. Only the exact reviewed generation candidate, landing
through `coop fork merge`, completes the canonical task. Don't run an older copied-queue worker
beside the new scheduler.

Current detached workers use `owner-v2` state, which carries an immutable fork generation. co:op
still reads `owner-v1`, but only so it can safely handle an older worker that is stopped or
pending cleanup. An `owner-v1` worker can't claim current task work or attach to a replacement
workspace. Never edit a pidfile to add a generation. Stop the old worker, let co:op bind a fresh
generation, and restart it.

Remote sessions created before their fork generation was persisted need the same care. Before you
upgrade, finish or discard them with the version that created them, and preserve any Git work you
want to keep.

On first start, the new co:op adopts a generationless session only when an exact host-owned
reservation already names that same session. That's the crash-safe partial-upgrade case.
Otherwise co:op quarantines the record. Its session and turn history stays readable, but co:op
doesn't run turns, inspect or review the workspace, clean its runtime or services, close it or
discard it. This keeps a deleted and recreated fork with the same name from being mistaken for the
old session.

For a quarantined session:

1. Inspect and preserve the quarantined workspace directly.
2. Create a new remote session. Never fabricate a generation or reservation file to make the old
   record attach.
3. When you're done with the record, retire it. `POST /v1/sessions/<id>/discard` with
   `{"retire_quarantined":true,"expected_revision":<n>}` tombstones the row and leaves the
   workspace to you.

The same applies to a session that co:op quarantines later because its workspace vanished.

### Strict `coop.conf`, one removal verb and supported runtimes

co:op validates `coop.conf` on every command. An unknown key, a duplicate key, a malformed line or
a retired key stops co:op before any work, and the error names the file and line. Replace the
retired loop settings with their `.agent/loop.yaml` fields:

| Retired `coop.conf` key | `.agent/loop.yaml` field |
| --- | --- |
| `COOP_LOOP_MODEL` | `work.agent` |
| `COOP_REVIEW_MODEL` | `signoff.agent` |
| `COOP_MAX_REVIEW_ROUNDS` | `signoff.rounds` |
| `COOP_LOOP_CMD` | `work.command` |
| `COOP_PREFLIGHT` | `preflight.enabled` |

Remove `COOP_AGENT_PACKAGES`. Images that co:op manages use locked client versions. If you need
different tooling, review a custom image and select it explicitly instead of overriding packages.

Project networking uses `box.egress: offline` instead of `none`, so update `.agent/project.yaml`.
Controller JobSpec documents still use `none` for their offline mode.

Restricted project images need an explicit build. Review the project's build inputs, then run
`coop build --egress filtered` before you reuse that image under filtered networking. Old
automatic build records don't authorize restricted reuse.

Removal and privacy flags now have explicit names. Replace these commands and flags:

| Retired | Use |
| --- | --- |
| `coop down -v` or `--volumes` | `coop down --delete-volumes` (still deletes stored service data) |
| `coop net export --include-destinations` | `--include-addresses` (still exposes hostnames/IPs) |
| `coop tasks clear` | `coop tasks rm --all-done` (the old alias is gone) |
| `coop net approve` | `coop approve` |
| `coop net explain` | `coop net blocked` |

`coop tasks split` is gone. Parallel forks share the canonical queue, as
[Canonical tasks across isolated forks](#canonical-tasks-across-isolated-forks) above explains.

`coop tasks flags` is gone too, with no replacement. No tagged release shipped it. A `flags.json`
that an untagged build left in a task folder does nothing now, and co:op leaves it in place.

Automatic runtime detection now prefers Docker. On a machine with both Docker and Apple
`container` installed, co:op picks Docker. Before, `container` won on name order. Your box image
must exist on the runtime you end up on, so if your images lived on the other one, run
`coop build` once after you upgrade. To keep Apple `container`, set `COOP_RUNTIME=container`.
co:op falls back to `container` only when Docker's daemon doesn't answer, and it tells you when it
falls back.

Podman is no longer a container runtime. Install Docker, or Apple `container` on macOS 26, then run
`coop build && coop doctor`. Auto-detection no longer looks for Podman, and an explicit
`COOP_RUNTIME=podman` now stops with the reason instead of running. co:op no longer manages
anything on the Podman side, so stop leftover sibling stacks there first:
`podman compose -p <project> -f .agent/compose.yml down --remove-orphans`.

The session state root moves to schema 25. Back it up before the new connection starts its
service, as [the controller cutover](#controller-owned-workers-and-session-schema-25) above
describes.

### One composition model, direct fork loops

Fusion was a second command grammar over capabilities co:op already exposes directly. co:op
removes the command and its mandatory "consult everyone before every action" governor prompt.
Presets, named peers, roles and consultation all stay.

| Retired | Use |
| --- | --- |
| `coop fusion <target> --peer <target>...` | `coop <target> --peer <target>...`: named peers remain read-only, explicit, and optional |
| `coop fusion <preset>` | `coop <preset>`: the preset lead, native/consult/delegate roles, ladders, and personas are unchanged |
| `coop acp fusion <target> --peer <target>...` | `coop acp <target> --peer <target>...` |
| `coop acp fusion <preset>` | `coop acp <preset>` |

Plain `coop acp` again starts automatically with the first signed-in provider, in Claude, Codex,
Gemini, Grok order, and its default account. An editor entry can use `["acp"]` and choose through
the live Preset, Provider and Account selectors. Explicit targets and presets still take
precedence, and `--bare` still requires a single explicit target. Preset ladders still rotate
across providers and accounts. Filtered sessions offer only compatible providers and whole
presets. An unrelated signed-in provider no longer prevents a supported lead from starting.
`coop-consult` still provides read-only fresh/continue sessions and target fallback for named peers
and preset consult roles.

Before the first launch with MCP after you upgrade, prepare your MCP config files:

- Make the configured `COOP_MCP_FILE` a private readable regular file no larger than 4 MiB.
- Set `COOP_MCP_FILE` to a canonical path without `..`.
- Shared MCP also inspects the selected Codex or Grok native config, so prepare that file the same
  way.
- co:op projects Gemini's native `settings.json` even without shared MCP, so that file must be safe
  before any Gemini launch.
- Replace a final symlink with a private regular copy instead of keeping the link.
- Keep the shared source outside repositories, companion repositories, credential homes and ACP
  session stores that co:op mounts wholesale.

co:op leaves an unsafe input untouched and refuses the launch it affects. After you correct the
file or source path, retry the original command.

A project Dockerfile selected by `.agent/Dockerfile` or `box.dockerfile` must now be a regular file
inside the repository. If it's a symlink, replace it with a regular copy before `coop build`.

Fleet was a declarative wrapper over the fork and loop commands. co:op removes its command family,
its live board and its `.agent/fleet.yaml` parser, and keeps the direct primitives:

| Retired | Use |
| --- | --- |
| `coop fleet init` / `.agent/fleet.yaml` | No manifest. Start each fork explicitly with `coop fork <name> <target\|preset> --loop -d --tasks <path>`. co:op doesn't delete an existing ignored file; remove it manually after translating its entries. |
| `coop fleet up` | Run the direct detached fork command once per worker. Point workers at the same canonical queue, or use `--tasks <path>` only to select a genuinely separate canonical queue. |
| `coop fleet down` | `coop fork stop <name>` for each running fork. |
| `coop fleet watch` | `coop tasks watch` for merged task progress; `coop fork ls` for fork state/cost; `coop fork logs -f` for output. |
| `coop fleet prune` | `coop fork rm <name>` for each obsolete fork (`--yes` confirms non-interactively; `--force` separately overrides dirty/unmerged protection). |

`coop fork merge --all` still lands every fork through a revalidating rebase queue. There is no
replacement manifest or batch up/down command, so start and stop each worker explicitly.

co:op also no longer inspects or removes basename-only Compose projects that were created before
per-workspace hashed project names. Finish or stop sibling services before you upgrade. If one of
those old stacks remains afterwards, inspect it with `docker compose ls`, then run
`docker compose -p <legacy-project> -f .agent/compose.yml down --remove-orphans`. co:op manages
only projects named by the current `ComposeProject(workspace)` scheme.

Fork session re-entry uses one record, `.coop/session.<provider>.<account>`. co:op ignores the
older provider-only `.coop/session.<provider>` file, and no longer adopts the latest Codex session
by cwd. The first re-entry without a current exact hint starts a fresh conversation. When the
provider creates one unambiguous session, co:op records an exact hint for later resumes. Remove the
provider-only files when it suits you. co:op will neither read nor rewrite them.

`coop init` now maintains only the current scaffold, and doesn't rewrite files that versions before
v8 generated. If you're upgrading from a version older than v8, make these changes yourself:

- In `.githooks/prepare-commit-msg`, replace `$HOME/.config/coop/git-hooks/prepare-commit-msg`
  with `$HOME/.coop-git-hooks/prepare-commit-msg`, then run
  `chmod +x .githooks/prepare-commit-msg`.
- If `.agent/rules/` exists, run `mkdir -p .agent/kb` and `git mv .agent/rules .agent/kb/rules` so
  the rule cards stay tracked at their current path. Then update project-owned instructions, such
  as `AGENTS.md`, to refer to `.agent/kb/rules/`.
- In `.gitignore`, remove the old co:op stanza that contains `.agent/*` or `**/.agent/*` together
  with `!.agent/rules/` or `!**/.agent/rules/`. Then run `coop init` once to append the current
  monorepo-aware stanza.

Existing project-owned hooks and custom hook paths stay protected. Instead of overwriting them,
`init` describes how to chain co:op's current hook.

Audit-reopen authority is also current-only. Active records remain version 3, and non-authorizing
pending records remain version 4. No tagged co:op release wrote the retired v1/v2 formats, so
upgrades between released versions need no conversion. The now-unused
`coop tasks unblock --adopt-audit-head` bridge has no replacement.

If an untagged developer build left a v1/v2 record, reconcile that task with the build that wrote
it before you upgrade. Current co:op leaves the record intact and refuses to lease, complete or
unblock the task. Don't delete raw task-authority registry files to get around that refusal.

## v7: the box Dockerfile moves into `.agent/`

v7.0.0 moved the box Dockerfile out of the repository root. co:op builds the box from
`.agent/Dockerfile` and no longer reads a root `Dockerfile.agent`. A repo that still has one gets
the standard box, without your toolchain, so move the file and rebuild:

```sh
mkdir -p .agent
git mv Dockerfile.agent .agent/Dockerfile
coop build
```

To keep the file where it is, set `box.dockerfile` in `.agent/project.yaml` to `Dockerfile.agent`
instead. That key takes any regular file inside the repository, such as your app's own `Dockerfile`.

The v7 box image also gained `socat`. It lets the box reach a sidecar's exposed port at
`localhost:<port>`. Run `coop build` or `coop update` once to rebuild the box with it.

## v4: the target grammar, one way to name a run

A target names who runs: `provider[:model][/effort][@account]`. For example: `claude`,
`claude:opus`, `claude/xhigh`, `claude:opus/xhigh`, `claude@work`, `claude:opus@work`.

A target must name the provider. The model, the account and a reasoning `/effort` are optional. The
effort (`low`/`medium`/`high`/`xhigh`/`max`) is passed straight to the agent's CLI. At v4 Gemini had
none and rejected it; since v10.1.2 it takes `low` or `high`. `--model`, `--credential` and the
boolean `--consult` retire, and you name peers explicitly.

| Retired | Use |
| --- | --- |
| `coop <agent> --model <m>` | `coop <agent>:<m>`: for example `coop claude:opus` |
| `coop <agent> --credential <acct>` | `coop <agent>@<acct>`: for example `coop claude@work` |
| `coop login <agent> --credential <acct>` | `coop login <agent>@<acct>` |
| `coop loop --model m@work` | `coop loop <agent>:m@work` (account ladder: `<agent>@work,personal`) |
| bare `coop` / `coop loop` (defaulted to claude) | name the target: `coop claude`, `coop loop claude` (or, since v5.0.0, positional `coop loop <preset>`, whose lead supplies it) |
| `coop <agent> --consult` (boolean) | `coop <target> --peer <target>...`: name each peer (repeatable): `--peer codex:gpt-6-astra --peer gemini`. v4 named peers with `--consult <peer>`, and v5.0.0 renamed it `--peer` |
| `coop fusion <target>` (consulted every signed-in agent) | `coop <target> --peer <target>...`: name only the peers this run may consult (v10.1.2 removed `coop fusion`) |

The target grammar applies on every current launch surface: `coop <target>`, `loop`, `acp`,
`fork <name> [acp]` and `login`. A Zed `agent_servers` entry can name a target as one token,
`["acp","claude:opus@work"]`, or, since v5.0.0, use `["acp"]` for automatic startup and live
selection.

A peer takes part only when you name it. The old "every signed-in agent is a peer" policy is gone.
A named peer's credentials are the only ones mounted for consultation, and the box's
`coop-consult` refuses any other. So an overnight run can't hand your Codex login to a Claude lead
that you never asked to consult it.

Since v5.0.0, name a preset in the positional who-runs slot, as in `coop <preset>` or
`coop loop <preset>`, instead of with `--preset`. A preset is a separate axis (role wiring), and
not another spelling of the target.

Presets follow the same grammar. `agent:` holds a target or a target ladder, and native roles
remain one Claude target. The separate `model:`/`models:` keys retire:

| Retired preset shape | Use |
| --- | --- |
| `lead: {agent: claude, models: [fable, opus@work]}` | `lead: {agent: [claude:fable, claude:opus@work]}`: one `agent:` ladder (each entry a target) |
| a role's `agent: codex` + `model: gpt-6-astra` | `agent: codex:gpt-6-astra`: the model rides `agent:` (a role runs its default account; no `@account`) |

A lead ladder can be cross-provider, as in `agent: [claude:opus, codex:gpt-6-astra]`. On a rate
limit, the loop rotates across vendors and runs each rung's agent. Since v5.0.0, an ACP session
rotates too: it re-creates the session on the new provider, and carries the conversation over
best-effort as a labeled plain-text preamble. The lead is the first rung's provider. It's the
default agent, and the one a single run uses.

Since v5.3.0, consult and delegate role ladders fail over inside their wrappers after a proven
non-zero rate-limit response. Native roles remain one target, because subagent frontmatter has no
runtime fallback hook. Role rungs always use each provider's default account, and `@account`
remains lead-only. co:op skips providers you haven't signed in to, and mounts every available
rung's credential home in the lead box.

## v3: retired command aliases

v3 has a clean CLI with no backward-compatible aliases. Each retired form is now unknown or
tombstoned, so rewrite it:

| Retired | Use |
| --- | --- |
| `coop clone <name>` | `coop fork <name>` |
| `coop profiles …` | `coop credentials …`: a credential is a stored account/login; orchestration recipes are presets (`coop help presets`) |
| `--profile <name>` (login/launch flags) | put the account in the target: `<agent>@<name>` (see the target-grammar section above). `--profile` is no longer a coop flag at all: on an agent launch it forwards to the agent like any other arg (codex has its own `--profile`); elsewhere it's an unknown argument |
| `coop pool <add\|rm\|clear>` | Retired: there is no persistent pool. A loop rotates its preset lead's `agent:` target ladder (`coop help presets`); a bare `provider:model` rung in that ladder fans out across every signed-in account, which is what the pool used to do. A stray `pools.json` is ignored. |
| `coop profiles <default\|rm> <agent> <name>` (verb-first) | `coop credentials <agent> <name> <default\|rm>` (a path) |
| `coop profiles <name> model <m>` / a credential's model mark | Retired: a credential is just an account; the model is a separate axis. Set it inline in the target (`<agent>:<m>`) or in a preset lead's `agent:` target ladder (`coop help presets`). Both spellings of `coop credentials <cred> model` tombstone. |
| `coop status` | `coop tasks watch` (the queue + any active forks) / `coop fork ls` (fork state) |
| `coop tasks start <id>` | `coop tasks claim <id>` |
| `coop loop --debug` | `coop loop --debug-on-fail` |
| `<any> list` (e.g. `coop tasks list`) | `<any> ls`: `ls` is the only list verb |
| `<any> remove` (e.g. `coop tasks remove`) | `<any> rm`: `rm` is the only destructive verb |

## Monorepos: a hand-set `COOP_TASKS` → `.agent/project.yaml`

This change isn't breaking: `COOP_TASKS` still works, and still overrides. If you exported
`COOP_TASKS="portal/.agent/tasks runner/.agent/tasks …"` so co:op would see a monorepo's queues,
you can delete the export. Commit a top-level `.agent/project.yaml` that lists the members:

```yaml
subprojects: [portal, runner, mcp, packs]
```

Every task command derives the queue set from it: each member's queue, plus the root's own for
changes that span members.

`coop init` at the root writes the file for you. It detects direct child directories that have a
`.agent/`, and scaffolds any member that's missing its queue.

## A legacy `.agent/TASKS.md` → the folder task system

Older co:op repos kept the work queue in a single `.agent/TASKS.md`, with `[ ]`/`[w]`/`[x]`/`[B]`
checkboxes, plus a global `.agent/PENDING_DECISIONS.md`. Since co:op v3, that layout is no longer
read. Each task is now a folder under `.agent/tasks/`, and a task's state is its directory:
`00_todo/`, `10_in_progress/`, `50_blocked/` or `99_done/`. The numeric prefix just sorts `ls` in
lifecycle order. There is no fallback, so convert once with the prompt below.

The conversion is a one-time migration that keeps all task content. An LLM handles it well,
because the old task bodies are prose that needs mapping rather than a rigid parse.

1. Commit (or stash) first, so the conversion is easy to review as a diff.
2. Paste the prompt below into any coding agent (Claude, Codex, Gemini, …) running in the repo.
3. Afterwards, verify with `coop tasks` and `coop tasks lint`.

```text
Convert this repo's legacy coop task queue to the folder-based format. Work
carefully and lose no task content.

SOURCE
- `.agent/TASKS.md`: each top-level line `- [ ] / [w] / [x] / [B] <title>` is one
  task; the indented bullets beneath it are that task's body.
- `.agent/PENDING_DECISIONS.md` (if present): human decisions, each tied by its text
  to a blocked task.
- Ignore the header/legend comments, the `[E]` example task, and any `- [ ]` lines
  inside ``` fenced code blocks (those are documentation, not tasks).

TARGET — a folder per task; the task's STATE is its directory (the NN_ prefix is
part of the directory name — use it verbatim):
  `- [ ]` → `.agent/tasks/00_todo/`        `- [w]` → `.agent/tasks/10_in_progress/`
  `- [B]` → `.agent/tasks/50_blocked/`      `- [x]` → `.agent/tasks/99_done/`

FOR EACH TASK
1. id = `YYYY-MM-DD-<slug>`: use a date from the task body if it has one, else
   today; slug = the title lowercased with every run of non-alphanumeric characters
   replaced by a single `-`, trimmed, ≤ 48 chars. Make each id unique.
2. Write `.agent/tasks/<state>/<id>/task.md`:

   ---
   id: <id>
   title: <the task's one-line title>
   labels: []
   updated: <today, ISO-8601>
   ---

   # <title>

   **Context:** <the body's Context, or a one-line summary of the task>

   **Acceptance criteria:** <the body's "Acceptance checks", if any>

   **Approach:** <the body's "Implementation direction", if any>

   ## Subtasks
   - [ ] <each concrete sub-step found in the body>

   Map the old body's Context / Likely files / Implementation direction /
   Acceptance checks into these fields. Omit the Subtasks section if there are no
   steps. Put anything that doesn't fit under a trailing `## Notes` heading —
   never drop content. Do NOT add a `status:` field; the directory is the status.

3. For a `[B]` (blocked) task, also write `.agent/tasks/50_blocked/<id>/decision.md`:

   # Decision: <the open question>

   **Blocks:** this task (`<id>`).

   **The decision:** <what must be chosen>

   **Options:**
   - **A — <name>:** <consequence>
   - **B — <name>:** <consequence>

   **Recommendation:** <if the body or PENDING_DECISIONS suggests one>

   ---

   **Resolution:** <fill in if it was already answered, else leave empty>

   If `.agent/PENDING_DECISIONS.md` has an entry matching this task (by the title or
   topic it names), fold it into this `decision.md`. A pending decision that matches
   no task → create a new `50_blocked/` task for it.

CLEAN UP
4. Once every task and decision has been migrated, delete `.agent/TASKS.md` and
   `.agent/PENDING_DECISIONS.md`.

VERIFY
5. Run `coop tasks` (it should list the same number of tasks as the old file,
   grouped by state) and `coop tasks lint` (it must be clean). Then report a
   summary: tasks migrated per state, decisions folded in, and anything that did
   not map cleanly.
```

## A legacy `.agent/BACKLOG.md` → the backlog drawer

Older co:op repos kept unscheduled ideas in a single `.agent/BACKLOG.md`, with one `##` section
per idea. As of this release, the backlog is a task-folder drawer, `.agent/tasks/xx_backlog/`,
managed with `coop backlog`. When an idea is ready, you promote it with a folder move
(`coop backlog promote <id>`) instead of a hand-rewrite. `coop init` no longer writes
`BACKLOG.md`.

If you never used the file, there's nothing to convert: `coop backlog add` creates the drawer on
demand. Otherwise, migrate by hand. It's short, because a backlog is usually a handful of items,
and they're prose rather than structured data. For each `##` section in `.agent/BACKLOG.md`, run:

```text
coop backlog add "<the item's title>"
```

Then paste the section's notes into the new item's `task.md`. `coop backlog add` prints its
path. For a `— DEFERRED (<why>)` item, carry the reason
across. You can just drop a shipped or cancelled one. When every item has moved, delete
`.agent/BACKLOG.md` and verify with `coop backlog`.
