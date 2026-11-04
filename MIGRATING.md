# Migrating

## Unreleased: `coop codex -p` runs one prompt

`coop codex -p <name>` used to pick a Codex profile. It now runs `<name>` as a prompt, as `-p` does
on every other agent. To pick a profile, use `coop codex --profile <name>`.

## Unreleased: boxes use filtered networking by default

Boxes now use filtered networking unless you choose another mode. An agent can reach its provider
and the destinations you approved. A plain `coop run` or `coop shell` can reach only the destinations
you approved.

- For unrestricted access, add `--egress open` to the command (in an editor, to the agent arguments),
  set `COOP_EGRESS=open` in your environment or `coop.conf`, or set `box.egress: open` in the project
  and approve it with `coop approve`.
- Installing `.tool-versions` tools when the box starts and starting `.agent/compose.yml` services
  automatically both need open networking. Run with `--egress open`, or build the tools into
  `.agent/Dockerfile` and start services with `coop up`.
- A project with its own `.agent/Dockerfile` and no `box.egress` needs one more `coop build`. After
  that, co:op rebuilds those files itself when its client image changes.
- Filtered project images have new names, so an existing filtered project also needs `coop build`
  once. `coop update` no longer builds new or changed files.
- Filtered networking needs Docker. On Apple's `container`, choose `--egress open` or
  `--egress offline`, or set `COOP_EGRESS`.
- The first filtered run on a host prepares the network images and checks the host, as
  `coop net setup` does.

## Unreleased: the no-network mode is called offline

The command line and `COOP_EGRESS` now say `offline`, as `.agent/project.yaml` already did. co:op
refuses `none` with a message that names `offline`.

- Replace `--egress none` with `--egress offline` in scripts, aliases and editor agent settings
  (for example `["acp", "--egress", "offline"]`).
- Replace `COOP_EGRESS=none` with `COOP_EGRESS=offline` in your environment and `coop.conf`.
- Controller JobSpec documents and the session API keep `none` for their offline mode.

## v12.0.0: network records are renamed

co:op now names each project's network records by the project's folder.

- Run `coop net setup` once. This release's new images already need it, and co:op no longer reads
  the old setup record.
- Run `coop approve` once in each project that uses filtered networking, and review its access
  again. Until you do, its runs stop with "coop renamed its network records". The review shows
  what changed since you last approved. To take a project's access back instead, run
  `coop net forget`.
- A project whose access you took back with `coop net forget` stays that way.
- Create a new session to replace each filtered worker session from before the upgrade. The old one
  can't find its saved network rules, and restarting the worker doesn't help.
- A filtered project with its own `.agent/Dockerfile` needs `coop build --egress filtered` once.
- Copies of launch network rules that earlier versions saved stay on disk, because co:op can't
  tell which sessions still use them. Copies made by `coop net setup` are the exception: co:op
  removes them with their check's run record.

## v12.0.0: fork records move into Coop's private folder

co:op now keeps each project's fork records in `~/.local/state/coop/forks/<key>/` instead of
`<project>-forks/.coop`. Fork workspaces stay in `<project>-forks/`.

- Stop every co:op process that uses forks before you upgrade: fork loops, `coop fork <name> acp`
  sessions and the session service. The first command of the new version in a project moves its
  records, and an older process still running would keep writing to the old place, which the new
  version never reads.
- In each project that has forks, start a box or run `coop fork ls` once after upgrading. Do this
  before you start a box from a folder that holds several projects. Until a project's records move, a box
  that can write to that project's forks folder could change them.
- Forks need each fork's folder and your home folder on one disk. co:op refuses forks for a project
  on another disk (an external drive or a separate APFS volume) and names both folders. That
  project's old folder stays in place, so an older co:op can still use its forks. On a session
  worker, check before you upgrade that its state root (`--state`) is on the same disk as the
  worker's home folder, because its job forks live under the state root.
- To go back to an older co:op, move the folder back first:
  `mv ~/.local/state/coop/forks/<key> <project>-forks/.coop`. `<key>` is the SHA-256 of the
  project's real path: `printf %s "$(cd <project> && pwd -P)" | shasum -a 256`.

## v11.1.0: project image preparation

If you upgrade from v11.0.2 or earlier, run `coop update --box-only` once with the newly installed
binary. The older updater keeps running in its old process after installation, so that first update
can still build an ordinary image from the old definitions.

When you start a provider interactively with filtered networking in a host terminal, co:op may also
offer Build and continue. The default answer is No. Review the saved snapshot that the prompt shows,
because a yes approves only those build files, not future edits. Headless commands, editor
connections, loops and controller jobs don't get this prompt. Project builds still use ordinary
networking, whatever network restrictions apply to the running box.

## v11.0.0: upgrading from v10.1.2

Coding and editor sessions now each use a complete native home for their repository: a folder with
the provider's own history, settings and other files. An ordinary home also belongs to the selected
account, while an editor home stays when you switch accounts. co:op copies native settings into a
new home once, and after that they stay local to that home.

The first time co:op uses a home, it imports only the older transcripts that it can prove belong to
the repository, and leaves the originals as they are. It keeps history that's ambiguous or
unsupported, and doesn't show it to another project. While older sessions are writing history, co:op
postpones the import without stopping them, so finish them and retry. An old source can't silently
bring back sessions you deleted natively.

co:op moves account credentials into one main store that stays on the host, and retires the old
credential sources only after it has saved the credentials in a form it can recover and has checked
for other writers. It doesn't copy refresh grants into repository homes, and the host renews them.
Online provider runs now require Docker, including open-network runs. Raw workloads without
credentials can still use Apple's container runtime.

To sign in, run `coop login <provider>@<account>` on the host. To remove an account, run
`coop credentials <provider> <account> rm`, which asks you to confirm. Removal deletes that
account's ordinary repository histories, but not editor conversations.

Logging in or out with the native client inside a box isn't a supported way to manage the shared
account. If it changes local credential files, co:op refuses to reuse that home, and its error names
the native home and the home's recovery directory, labeled "recovery custody". To recover:

1. Stop the sessions that use that home.
2. Back up the entire recovery directory, including `home/`, `owner.json`, auth and import receipts.
3. To recover credentials that were created locally, sign in explicitly on the host.
4. Restore public auth files from a known-good backup, or explicitly move the entire backed-up
   recovery directory aside to start fresh. If you move only `home/`, the ownership receipts stay
   behind and co:op refuses to start.

Keep the backup. A fresh home doesn't prove that the old home's unique history has been recovered.

Before you upgrade an existing installation, finish older sessions and keep a verified backup of
its stopped credential and history roots. After co:op moves the credentials, don't run older
binaries against those roots. A rollback must use the current stored credentials or a fresh sign-in
on the host, because restoring tokens from before a refresh can invalidate the account access you
have left. Old binaries and native clients on the host don't take part in co:op's checks for other
writers.

## v10.1.2: upgrading from v8.1.0

These steps cover the changes since v8.1.0. Do the ones that apply to you before you use the new
binary to start workers, fork loops or sessions with MCP.

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
controller CA. The connection owns its local service: it refuses a root that's already listening,
and never reuses or replaces it. `coop worker`, `sessions serve`, `sessions policies` and
`connect --config` are gone, and there's no worker JSON file or local execution-policy catalog to
translate.

Controllers must send worker protocol v2 and canonical version-2 JobSpec documents, and select
workers that advertise live `job-setup:2`. Each job freezes its setup: source and companions,
targets, mode, networking, explicit `environment`, `check.argv` and `check.environment` values, and
positive CPU, memory and PID caps. Work and review use the same frozen setup, and check-only
environment values override work values. Resolve repository defaults before you submit a job,
because worker `COOP_GATE` and repository `gate:` no longer choose controller reviews.
[The session API](docs/session-api.md#sessions) lists the fields, limits and digest requirements.

You can still read older jobs. A job without the current setup can't run new turns or reviews, and
can't finish a session creation that was interrupted. For each of those jobs:

1. Preserve the work and history you need.
2. Where you can, finish, close or discard it through the supported old workflow.
3. Resubmit unfinished work as a new v2 job.

Never edit saved documents or create your own permission records to make an old record run.

Sign in to the provider accounts on each worker with co:op's normal login flow. Controllers don't
transfer model credentials.

### Anchored fork and network identity

co:op no longer identifies a fork or an approved project by directory metadata, because Linux
overlay filesystems can reuse all of it, including the inode and birth time, right after a
deletion. Fork generations and project network approvals now use a filesystem anchor: two hardlinks
to one file.

A fork has a `.coop-fork-generation` file, linked to a file in the fork state beside it. A reviewed
project has a `.coop-network-approval` file, linked to a file in co:op's network state. Only the
owner can access the fork state and the network state, and the visible files hold identifiers and no
credentials. Whenever co:op uses an anchor, both names must still be the same owner-controlled
`0600` inode with exactly two links.

The two names must be on one filesystem that supports hardlinks. Fork state sits next to its
workspace, so for forks this is normally automatic. For network approvals, keep the checkout and
`$XDG_STATE_HOME` (or the default `~/.local/state`) on the same filesystem. If co:op can't create
the link, it reports a clear error and doesn't fall back to timestamps.

co:op migrates a stopped v1/v2 fork record to a hardlink anchor only when all of these are true:

- Its Git branch and absolute local origin still match.
- It has no execution record and no unfinished merge.
- It has no reservation.

co:op never guesses ownership from a missing or foreign reservation, or from a live execution. It
quarantines a remote session with an old reservation, and
[Canonical tasks across isolated forks](#canonical-tasks-across-isolated-forks) says what to do
with it. If a legacy fork is still active, stop it and retry.

Older network approvals grant nothing under the new binary. Run `coop approve` once to review and
enroll the current project. Task records and remote-session discard plans keep their format.

Ordinary Git status doesn't show the markers, but a manual `git clean -x` can still remove them. To
repair a damaged project approval:

1. Inspect it with `ls -l -- ./.coop-network-approval`.
2. Once you've verified that the damaged name is stale, run `rm -- ./.coop-network-approval`.
3. Run `coop approve`.

If you move an intact checkout to a new path, run only `coop approve`. co:op verifies and retires
the old pair before it enrolls the new path. Copying the marker's bytes never transfers access, and
never retires the original project's anchor.

To repair a damaged fork marker:

1. Preserve any Git work, task notes or other data you need.
2. Remove the fork and its anchor with the normal `coop fork rm <name> --force` flow, which asks you
   to confirm. This permanently deletes the fork's unmerged work and service volumes, so inspect the
   deletion preview before you confirm.
3. Recreate the fork.

Don't remove only `.coop-fork-generation`, because the generation record and the private link must
retire together.

When an anchor exists, runtime arguments may mount the exact project. They may not mount:

- a path inside the project that an existing agent could swap before the runtime resolves the bind
- a writable folder above the project that could replace it

No mount may expose co:op's private anchor, execution-record or launch-lock state, even read-only.
That includes paths the first approval or fork will create. co:op refuses `--volumes-from`, whose
sources it can't inspect, and custom volume drivers. It inspects existing named volumes before
launch. To mount a cache, put it outside the checkout and name its path explicitly, so co:op can
inspect it.

Saved service-file approvals from earlier formats, including the first repository-scoped format,
no longer grant access. Run `coop up` at a terminal to review eligible read-only secret files
again. You can't approve secret directories or writable binds.

An external or custom-named Docker volume now also needs an explicit review at a terminal.
Automatic, filtered and non-terminal starts refuse such a volume and stop, even after approval. Your
approval applies only to the selected Docker daemon and the inspected plain-local volume object, so
switching the daemon or replacing a volume needs another review.

The approval belongs to the repository's `.coop-service-approval` marker and a private hardlink in
co:op state. Keep both on one filesystem. Copying the marker into another checkout doesn't copy the
approval. Inspect a damaged marker before you remove it and review again.

co:op now refuses sidecar repository binds that use SELinux relabel or propagation options. Remove
those options before you retry. A read-only session also requires its bind source to exist
already, because Compose's short syntax would otherwise create a directory on the host.

### Canonical tasks across isolated forks

Fork loops now take their tasks from the project's canonical task queue instead of copying a
complete `.agent/tasks` tree into each fork, and `coop tasks split` is retired.

Before you upgrade:

1. Stop every detached fork loop with the co:op version that started it.
2. Inspect each fork's Git work and copied task folders.
3. Preserve any fork-only notes, decisions, artifacts or unfinished changes before you start the
   new loop. The new controller won't choose which of two copied folders is correct.

The first new loop on a fork refuses to start while that workspace still has tasks under its own
`.agent/tasks`, including configured subproject queues. Review and reconcile those copies, then
recreate the fork with `coop fork <name> <target> --fresh --loop`. Add `--force` only after you've
reviewed any unmerged Git work and decided to discard it.

co:op doesn't import or delete old `.agent/tasks.sliceN` directories in the canonical checkout.
Turn work that's really separate into canonical tasks yourself, and archive the old slice only
after you've verified that every note and change has a home.

After you migrate, start any number of forks against the same queue:

```sh
coop fork perf codex --loop -d
coop fork docs claude --loop -d
coop tasks watch
```

`--tasks <path>` now selects a canonical queue, not a place to copy tasks to. The host gives each
fork one assignment, which survives a stop or a crash, and shows the fork only that task in
`.coop/task-executions/`. The canonical task completes only when the exact candidate reviewed for
that fork generation lands through `coop fork merge`. Don't run an older worker that uses a copied
queue beside the new scheduler.

Current detached workers use `owner-v2` state, which records a fork generation that can't change.
An `owner-v1` worker can't claim current task work or attach to a replacement workspace, and co:op
reads its state only to handle an older worker that's stopped or waiting for cleanup. Never edit a
pidfile to add a generation. Stop the old worker, let co:op bind a fresh generation, and restart it.

Remote sessions created before their fork generation was saved need the same care. Before you
upgrade, finish or discard them with the version that created them, and preserve any Git work you
want to keep.

On first start, the new co:op quarantines the record of each such session. Its session and turn
history stays readable, but co:op doesn't run turns, inspect or review the workspace, clean its
runtime or services, close it or discard it. This keeps a deleted and recreated fork with the same
name from being mistaken for the old session.

For a quarantined session:

1. Inspect and preserve the quarantined workspace directly.
2. Create a new remote session. Never fabricate a generation or reservation file to make the old
   record attach.
3. When you're done with the record, retire it. `POST /v1/sessions/<id>/discard` with
   `{"retire_quarantined":true,"expected_revision":<n>}` marks the record as retired and leaves the
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

Remove `COOP_AGENT_PACKAGES`, because images that co:op manages use locked client versions. If you
need different tooling, review a custom image and select it explicitly.

Project networking uses `box.egress: offline` instead of `none`, so update `.agent/project.yaml`.
Controller JobSpec documents still use `none` for their offline mode.

Restricted project images need an explicit build. Review the project's build inputs, then run
`coop build --egress filtered` before you reuse that image under filtered networking. Old
automatic build records don't allow that reuse.

Removal and privacy flags now have explicit names. Replace these commands and flags:

| Retired | Use |
| --- | --- |
| `coop down -v` or `--volumes` | `coop down --delete-volumes` (still deletes stored service data) |
| `coop net export --include-destinations` | `--include-addresses` (still exposes hostnames/IPs) |
| `coop tasks clear` | `coop tasks rm --all-done` |
| `coop net approve` | `coop approve` |
| `coop net explain` | `coop net blocked` |

`coop tasks flags` is gone, with no replacement. No tagged release shipped it. A `flags.json` that
an untagged build left in a task folder does nothing now, and co:op leaves it in place.

On a machine with both Docker and Apple `container` installed, automatic runtime detection now picks
Docker, where it used to pick `container`. Your box image must exist on the runtime you end up on,
so if your images lived on the other one, run `coop build` once after you upgrade. To keep Apple
`container`, set `COOP_RUNTIME=container`. co:op falls back to `container` only when Docker's daemon
doesn't answer, and it tells you when it falls back.

co:op no longer supports Podman as a container runtime. Install Docker, or Apple `container` on
macOS 26, then run `coop build && coop doctor`. Auto-detection no longer looks for Podman, and an
explicit `COOP_RUNTIME=podman` now stops with the reason. co:op no longer manages anything on the
Podman side, so stop leftover sibling stacks there first:
`podman compose -p <project> -f .agent/compose.yml down --remove-orphans`.

### One composition model, direct fork loops

co:op removes `coop fusion` and its mandatory "consult everyone before every action" governor
prompt. Presets, named peers, roles and consultation all stay.

| Retired | Use |
| --- | --- |
| `coop fusion <target> --peer <target>...` | `coop <target> --peer <target>...`: named peers remain read-only, explicit, and optional |
| `coop fusion <preset>` | `coop <preset>`: the preset lead, native/consult/delegate roles, ladders, and personas are unchanged |
| `coop acp fusion <target> --peer <target>...` | `coop acp <target> --peer <target>...` |
| `coop acp fusion <preset>` | `coop acp <preset>` |

Plain `coop acp` again starts automatically with the first signed-in provider, in Claude, Codex,
Gemini, Grok order, and its default account. An editor entry can use `["acp"]` and choose through
the live Preset, Provider and Account selectors. Explicit targets and presets still take
precedence, and `--bare` still requires a single explicit target. Filtered sessions offer only
compatible providers and whole presets. An unrelated signed-in provider no longer prevents a
supported lead from starting.

Before the first launch with MCP after you upgrade, prepare your MCP config files:

- Make the configured `COOP_MCP_FILE` a private readable regular file no larger than 4 MiB.
- Set `COOP_MCP_FILE` to a canonical path without `..`.
- Shared MCP also inspects the selected Codex or Grok native config, so prepare that file the same
  way.
- co:op reads Gemini's native `settings.json` and generates the box's version even without shared
  MCP, so that file must be safe before any Gemini launch.
- Replace a final symlink with a private regular copy.
- Keep the shared source outside repositories, companion repositories, credential homes and ACP
  session stores that co:op mounts as a whole.

co:op leaves an unsafe input untouched and refuses the launch it affects. After you correct the
file or source path, retry the original command.

A project Dockerfile selected by `.agent/Dockerfile` or `box.dockerfile` must now be a regular file
inside the repository. If it's a symlink, replace it with a regular copy before `coop build`.

co:op removes Fleet's commands, its live board and its `.agent/fleet.yaml` parser. Use the direct
fork and loop commands:

| Retired | Use |
| --- | --- |
| `coop fleet init` / `.agent/fleet.yaml` | No manifest. Start each fork explicitly with `coop fork <name> <target\|preset> --loop -d --tasks <path>`. co:op doesn't delete an existing ignored file, so remove it manually after you translate its entries. |
| `coop fleet up` | Run the direct detached fork command once per worker. Point workers at the same canonical queue, or use `--tasks <path>` only to select a genuinely separate canonical queue. |
| `coop fleet down` | `coop fork stop <name>` for each running fork. |
| `coop fleet watch` | `coop tasks watch` for merged task progress, `coop fork ls` for fork state and cost, and `coop fork logs -f` for output. |
| `coop fleet prune` | `coop fork rm <name>` for each obsolete fork (`--yes` confirms without a prompt, and `--force` separately overrides the protection for dirty or unmerged forks). |

`coop fork merge --all` still lands every fork through a rebase queue that checks each fork again.
There's no batch up/down command, so start and stop each worker explicitly.

co:op also no longer inspects or removes Compose projects named only after the folder, which date
from before project names included a hash for each workspace. Finish or stop sibling services
before you upgrade. If one of those old stacks remains afterwards, inspect it with
`docker compose ls`, then run
`docker compose -p <legacy-project> -f .agent/compose.yml down --remove-orphans`.

When you re-enter a fork session, co:op uses one record, `.coop/session.<provider>.<account>`. It
ignores the older provider-only `.coop/session.<provider>` file, which you can remove whenever you
like, and no longer adopts the latest Codex session by working directory. The first re-entry without
a current exact hint starts a new conversation. When the provider creates one unambiguous session,
co:op records an exact hint for later resumes.

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

Audit-reopen records also support only the current formats. No tagged co:op release wrote the
retired v1/v2 formats, so upgrades between released versions need no conversion. The now-unused
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

The v7 box image also gained `socat`, which lets the box reach a sidecar's exposed port at
`localhost:<port>`. Run `coop build` or `coop update` once to rebuild the box with it.

## v4: the target grammar, one way to name a run

A target names who runs: `provider[:model][/effort][@account]`. For example: `claude`,
`claude:opus`, `claude/xhigh`, `claude:opus/xhigh`, `claude@work`, `claude:opus@work`.

A target must name the provider. The model, the account and a reasoning `/effort` are optional.
co:op passes the effort (`low`/`medium`/`high`/`xhigh`/`max`) straight to the agent's CLI. At v4,
Gemini had no effort levels and rejected the setting. Since v10.1.2 it takes `low` or `high`.
`--model`, `--credential` and the boolean `--consult` are retired, and you name peers explicitly.

| Retired | Use |
| --- | --- |
| `coop <agent> --model <m>` | `coop <agent>:<m>`: for example `coop claude:opus` |
| `coop <agent> --credential <acct>` | `coop <agent>@<acct>`: for example `coop claude@work` |
| `coop login <agent> --credential <acct>` | `coop login <agent>@<acct>` |
| `coop loop --model m@work` | `coop loop <agent>:m@work` (account ladder: `<agent>@work,personal`) |
| bare `coop` / `coop loop` (defaulted to claude) | name the target: `coop claude`, `coop loop claude` (or, since v5.0.0, positional `coop loop <preset>`, whose lead supplies it) |
| `coop <agent> --consult` (boolean) | `coop <target> --peer <target>...`: name each peer (repeatable): `--peer codex:gpt-6-astra --peer gemini`. v4 named peers with `--consult <peer>`, and v5.0.0 renamed it `--peer` |
| `coop fusion <target>` (consulted every signed-in agent) | `coop <target> --peer <target>...`: name only the peers this run may consult (v10.1.2 removed `coop fusion`) |

The target grammar works in every current launch command: `coop <target>`, `loop`, `acp`,
`fork <name> [acp]` and `login`. A Zed `agent_servers` entry can name a target as one token,
`["acp","claude:opus@work"]`, or, since v5.0.0, use `["acp"]` for automatic startup and live
selection.

A peer takes part only when you name it. co:op mounts only a named peer's credentials for
consultation, and the box's `coop-consult` refuses any other peer, so an overnight run can't hand
your Codex login to a Claude lead that you never asked to consult it.

Since v5.0.0, name a preset in the position that says who runs, as in `coop <preset>` or
`coop loop <preset>`, instead of with `--preset`.

Presets follow the same grammar. `agent:` holds a target or a target ladder, and native roles
remain one Claude target. The separate `model:`/`models:` keys are retired:

| Retired preset shape | Use |
| --- | --- |
| `lead: {agent: claude, models: [fable, opus@work]}` | `lead: {agent: [claude:fable, claude:opus@work]}`: one `agent:` ladder (each entry a target) |
| a role's `agent: codex` + `model: gpt-6-astra` | `agent: codex:gpt-6-astra`: the model goes in `agent:` (a role runs its default account, with no `@account`) |

A lead ladder can span providers, as in `agent: [claude:opus, codex:gpt-6-astra]`. On a rate limit,
the loop rotates across vendors and runs each rung's agent. Since v5.0.0, an ACP session rotates
too, and carries the conversation over, best effort, as a labeled plain-text preamble. The lead is
the first rung's provider. It's the default agent, and the one a single run uses.

Since v5.3.0, consult and delegate role ladders fail over inside their wrappers after a non-zero
exit that co:op confirms is a rate limit. Role rungs always use each provider's default account,
and `@account` remains lead-only. co:op skips providers you haven't signed in to, and mounts every
available rung's credential home in the lead box.

## v3: retired command aliases

v3 has no backward-compatible aliases. Each retired form is now either unknown or stops with a hint,
so rewrite it:

| Retired | Use |
| --- | --- |
| `coop clone <name>` | `coop fork <name>` |
| `coop profiles …` | `coop credentials …`: a credential is a stored account or login, and orchestration recipes are presets (`coop help presets`) |
| `--profile <name>` (login/launch flags) | put the account in the target: `<agent>@<name>`. `--profile` is no longer a coop flag. On an agent launch, it goes to the agent like any other argument (codex has its own `--profile`). Anywhere else, it's an unknown argument |
| `coop pool <add\|rm\|clear>` | There is no persistent pool. A loop rotates through its preset lead's `agent:` target ladder (`coop help presets`). A bare `provider:model` rung in that ladder runs on every signed-in account, which is what the pool used to do. co:op ignores a stray `pools.json`. |
| `coop profiles <default\|rm> <agent> <name>` (verb-first) | `coop credentials <agent> <name> <default\|rm>` (a path) |
| `coop profiles <name> model <m>` / a credential's model mark | A credential is just an account, and the model is a separate choice. Set it in the target (`<agent>:<m>`) or in a preset lead's `agent:` target ladder (`coop help presets`). Both spellings of `coop credentials <cred> model` now fail. |
| `coop status` | `coop tasks watch` (the queue and any active forks) / `coop fork ls` (fork state) |
| `coop tasks start <id>` | `coop tasks claim <id>` |
| `coop loop --debug` | `coop loop --debug-on-fail` |
| `<any> list` (e.g. `coop tasks list`) | `<any> ls`: `ls` is the only list verb |
| `<any> remove` (e.g. `coop tasks remove`) | `<any> rm`: `rm` is the only destructive verb |

## Monorepos: a hand-set `COOP_TASKS` → `.agent/project.yaml`

This change isn't breaking: `COOP_TASKS` still works, and it still overrides `.agent/project.yaml`.
If you exported `COOP_TASKS="portal/.agent/tasks runner/.agent/tasks …"` so co:op would see a
monorepo's queues, you can delete the export. Commit a top-level `.agent/project.yaml` that lists
the members:

```yaml
subprojects: [portal, runner, mcp, packs]
```

Every task command gets its set of queues from this file: each member's queue, plus the root's own
queue for changes that span members.

`coop init` at the root writes the file for you. It detects direct child directories that have a
`.agent/`, and sets up any member that's missing its queue.

## A legacy `.agent/TASKS.md` → the folder task system

Older co:op repos kept the work queue in a single `.agent/TASKS.md`, with `[ ]`/`[w]`/`[x]`/`[B]`
checkboxes, plus a global `.agent/PENDING_DECISIONS.md`. Since v3, co:op no longer reads that
layout, and there's no fallback. Each task is now a folder under `.agent/tasks/`, and a task's state
is its directory: `00_todo/`, `10_in_progress/`, `50_blocked/` or `99_done/`.

Convert once with an LLM, because the old task bodies are prose that needs mapping and a rigid
parse won't work. The prompt below keeps all task content.

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
per idea. The backlog is now a task-folder drawer, `.agent/tasks/xx_backlog/`, that you manage with
`coop backlog`. When an idea is ready, promote it with `coop backlog promote <id>`. Don't rewrite
it by hand. `coop init` no longer writes `BACKLOG.md`.

If you never used the file, there's nothing to convert: `coop backlog add` creates the drawer when
you need it. Otherwise, migrate by hand. For each `##` section in `.agent/BACKLOG.md`, run:

```text
coop backlog add "<the item's title>"
```

`coop backlog add` prints the path of the new item's `task.md`. Paste the section's notes there.
For a `— DEFERRED (<why>)` item, copy the reason across too. You can just drop an item that shipped
or was cancelled. When every item has moved, delete `.agent/BACKLOG.md` and verify with
`coop backlog`.

## The /sweep Stop hook in older repos

The `/sweep` skill's `Stop` hook keeps only an active sweep running and holds no other session
open. New projects get this setup from `coop init`, but `init` preserves existing files, so
update a repo set up by an older co:op by hand:

1. Remove only the stock `Stop` group from `.claude/settings.json`. Keep `PreToolUse` and custom
   hooks.
2. Delete `.claude/hooks/stop-guard.sh`, but only if it's the old stock guard.
3. Remove a stale `.agent/active`.
4. Merge the current `.agent/skills/sweep/` files, keeping your local customizations.
