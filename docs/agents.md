# Agents, accounts and presets

co:op runs four coding agents, with a choice of accounts and models. You can add peers for second
opinions, use presets and provide instructions for each agent. For short introductions, see
[credentials](https://coop.dryga.com/docs.html#credentials),
[models](https://coop.dryga.com/docs.html#models),
[second opinions](https://coop.dryga.com/docs.html#peers) and
[presets](https://coop.dryga.com/docs.html#presets).

## Where agent settings live

Coop gives each repository and selected account its own provider folder. The box mounts it where
the provider normally keeps its files, so conversation history, saved sessions, settings and
indexes stay together.

| Provider | Home in the box | Settings inside that home |
| --- | --- | --- |
| Claude | `~/.claude` | `settings.json` |
| Codex | `~/.codex` | `config.toml` |
| Gemini | `~/.gemini` | `settings.json` |
| Grok | `~/.grok` | `config.toml` |

Under `~/.config/coop/agents/<agent>/`, co:op keeps these separate stores:

| Path | What it holds |
| --- | --- |
| `credentials/<name>/` | the account's credentials and sign-in records, kept only on the host |
| `native-homes/<name>/<repository-key>/home/` | the provider's history, settings and other files for this repository and account |
| `acp-homes/<repository-key>/home/` | one repository's editor-session files, kept when you switch accounts |
| `profiles/<name>/` | older history and host defaults kept for reuse; coding boxes never mount this folder |

Coop copies reviewed host defaults into a new provider folder once. Changes you make later stay
with that repository; changing the defaults does not overwrite an existing folder. Coop forks
that it has validated share their parent's history storage. Unrelated repositories keep separate
history.

## What each run can see

A box gets provider folders only for its lead, named peers and preset roles. Shell, raw-command
and maintenance boxes get none. Sign-in runs in a separate sign-in home for the selected
provider, with no project mounted.

API keys and OAuth access and refresh tokens stay outside coding boxes. A broker for each run
adds the credentials to requests to the selected provider, and the host renews expiring
credentials without stopping the run. The box receives only the public authentication values
the native client needs to use that broker.

All agents using the same provider in one box share its selected account. Put agents you do not
trust with each other's access in separate boxes.

### Blast radius

A compromised agent can read and change the history and settings saved for its current
repository. It can also use the selected account through the run's broker. A hook it adds to
those settings can run again in later boxes for the same repository and account. Keeping
repositories separate does not protect those local files from the agent or limit what it can
spend with the provider.

The provider folders Coop mounts do not give the agent access to another repository's provider
files. Open networking still lets it send traffic to any destination. Use filtered networking
to restrict destinations, or `COOP_EGRESS=none` to turn networking off. Offline runs receive no
provider credentials.

Manage shared accounts on the host with `coop login` and `coop credentials … rm`. Do not use a
provider's own login or logout command inside a coding box. If local authentication files no
longer match the expected account state, Coop stops the next launch and keeps the files so you
can recover them. See [migration and recovery](../MIGRATING.md).

## Authentication

### Sign in

Sign in with `coop login`:

```bash
coop login claude     # interactive login; credentials stay in host-only storage
coop login codex      # device-code login (the box has no browser for an OAuth redirect)
coop login gemini     # hidden API-key prompt; stored outside the mounted Gemini home
```

### API keys

Supported API keys work with both open and filtered Docker networking. Put a key in the shared
env file, or use the provider's host sign-in flow:

```bash
install -d -m 700 ~/.config/coop/agents
touch ~/.config/coop/agents/env
chmod 600 ~/.config/coop/agents/env
echo 'ANTHROPIC_API_KEY=sk-…' >> ~/.config/coop/agents/env
echo 'OPENAI_API_KEY=sk-…'    >> ~/.config/coop/agents/env
echo 'GEMINI_API_KEY=AIza…'   >> ~/.config/coop/agents/env
```

co:op lets only your user access the credential folder (`0700`) and its secret files (`0600`).

| Provider | Supported key | Refused alternatives |
| --- | --- | --- |
| Claude | `ANTHROPIC_API_KEY` | `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN` |
| Codex | `OPENAI_API_KEY` | `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN` |
| Gemini | `GEMINI_API_KEY` | Vertex `GOOGLE_API_KEY` |
| Grok | none | `XAI_API_KEY` |

Subscription and OAuth accounts also keep their credentials on the host. Loops, peers, presets,
editor sessions and remote sessions all use the broker. You can switch between API-key and
subscription accounts across runs without exposing either account's credentials to the box.
Offline runs remove provider keys and do not start a broker.

The broker needs a private Docker network namespace to keep provider credentials outside the
box. Online provider runs therefore cannot use host networking, join an arbitrary container's
network or use Apple's `container` runtime. Coop also refuses custom provider base URLs and
credential types it does not support. See
[provider networking](networking.md#provider-credentials-stay-outside-the-box).

### The env file

`KEY=value` sets a value directly. A bare `KEY` copies the value from the environment of the
process running co:op. If that variable is unset, co:op leaves it out. When entries repeat, the
last assignment or successful import wins.

A login supplied only through the env file appears as the provider's default credential in
`coop credentials`. It needs no credential marker file and represents only that default account.
To add named accounts, save a separate login for each one. When you run a stored named account,
co:op leaves that provider's token variables out of the run so they cannot override the selected
login. This also applies when the stored account is your default.

Non-credential variables such as `DATABASE_URL` and `COOP_NO_ASDF` are shared with every box.

### What the box receives

The box receives public authentication values for the provider's native client and a private
connection to the run's broker. Before launch, co:op removes reusable provider keys and tokens
from both the user and project environment. It refuses provider credentials passed through
runtime `-e`, which would expose them directly. Sign-in boxes also receive none of the existing
API keys from the shared env file or the host's account store.

### First run

On the first run, co:op answers the provider's setup prompts inside the box: Claude's theme,
folder-trust and bypass warnings, Codex's "trust this directory?", and Gemini's folder trust.
You do not need to repeat this setup after signing in. Coop treats the mounted repository as
trusted inside the container, where the box provides the sandbox.

## Several accounts and failover

You can save several accounts for one provider, each with a name and its own rate-limit allowance.
When one account reaches its limit, an unattended run can switch to another account and keep
working.

```bash
coop login claude@work        # a second account…
coop login claude@personal    # …and a third
coop credentials              # list them and which are signed in
coop usage                    # limits and a 30-day API estimate
coop usage codex@work         # include history and pricing details for one account
```

### Failover

When `coop loop` (or a `coop fork --loop`) reaches a rate or usage limit, it switches to the next
target. It waits only when every target has reached its limit. A
[Zed (ACP) session](integrations.md) also switches targets, sends your prompt again and updates
the toolbar selection. If every account is limited, it waits until the first limit resets.

The lead's `agent:` list sets the order of models and accounts to try. This fallback list is
called a ladder; you do not need to configure a separate account pool:

- With no preset, the loop rotates the agent's default model across every signed-in account.
- A bare model in a ladder does the same.
- A pinned `model@account` runs just that one account.

Limits are tracked per model and account pair, so `claude-opus-5-5@personal` stays usable while
`claude-opus-5-5@work` cools down. A ladder gives you fallbacks in the order you write them: step to
a cheaper model, another account, or both. For example, run `coop presets init my-ladder`, then edit
that preset's lead:

```yaml
# .agent/presets/my-ladder/preset.yaml (lead excerpt)
lead:
  # Opus on all accounts, then Fable on work.
  agent: [claude:claude-opus-5-5, claude:claude-fable-5-1@work]
```

```bash
coop loop my-ladder            # rotates that ladder; coop presets shows every recipe
coop loop claude:opus@work     # or a one-off single target, no preset
```

Switching accounts loses no work. Each loop iteration is a fresh run, and the queue plus git carry
the progress.

### Choose the default account

Mark an account as the default to choose which login a plain interactive `coop claude` uses.
Account names have no special meaning, so choose names that help you recognize them. Use
`coop credentials <agent> <credential> default` to set the default, or
`coop credentials <agent> <credential> rm` to remove an account:

```bash
coop credentials claude personal default  # `coop claude` now runs on the personal account
```

## Picking models

To choose a model for a run, include it in the target as `<agent>:<model>`. This works for
interactive sessions, loops, forks and editor connections:

```bash
coop claude:opus                                    # one big-model interactive session
coop claude:fable --peer codex --peer gemini        # the lead's model (peers keep their own)
coop loop claude:haiku                              # a cheap overnight grind
coop fork risky claude:opus                         # a careful fork on the big model
coop acp claude:sonnet                              # pin an editor entry's model
```

Save the model in a [preset](#presets) if you use it regularly. The lead's `agent:` list chooses
its model and, in a loop, the accounts and models to try when a limit is reached. Each role
chooses its own model. Credentials select the account and subscription; they do not select
the model:

```bash
coop models                        # the model menu per agent
coop claude:opus                   # one run on the big model
coop codex/high                    # default model at high reasoning effort
coop claude:opus/xhigh             # …at extra-high reasoning effort (low·medium·high·xhigh·max)
coop frontier                      # a standing lead model + roles, from the preset (its lead leads)
```

[`COOP_<AGENT>_MODEL`](configuration.md#environment-variables) sets the agent-wide
default, for example `COOP_CLAUDE_MODEL=fable`.

Use [`.agent/loop.yaml`](loop.md) to choose a model for each loop step. `work.agent` selects the
model that works on tasks. `signoff.agent` lets you use a stronger model for the final review
while keeping task work on a cheaper model. Choose each fork's target or preset in
`coop fork <name> <target|preset> --loop`.

If more than one is set, the most specific wins:

1. the target's `:model`
2. the preset ladder's active entry
3. `COOP_<AGENT>_MODEL`
4. a model baked into `COOP_<AGENT>_CMD`
5. the agent CLI's own default

### Reasoning effort

Set reasoning effort on the same target. Add `/effort` after the provider or the
optional model: `coop codex/high`, `coop codex:gpt-6-astra/high`,
`coop claude:opus/xhigh`, `coop loop claude:opus/low`. The levels are `low`, `medium`,
`high`, `xhigh` and `max`.

co:op passes the level to the agent's CLI, except for known unsupported model-and-effort combinations
which it refuses before starting:

| Agent | Where the level goes |
| --- | --- |
| Claude | `--effort` |
| Codex | `model_reasoning_effort` |
| Grok | `--reasoning-effort` |
| Gemini | its thinking setting (`low` or `high` only) |

Haiku 4.5 does not support reasoning-effort settings. Use `claude:haiku` without an effort suffix,
or a model that supports the setting, such as `claude:opus/low`. The native CLI can ignore an
effort setting for Haiku, while the editor adapter rejects it. Coop refuses the unsupported
combination before either one starts.

Gemini uses a thinking setting instead of an effort flag. Coop accepts `low` or `high` for
models it can map to that setting and checks the value before launch. These are the only levels
Gemini 3 supports, and the client can hand a turn from another Gemini model to Gemini 3.
A thinking setting you have explicitly saved for a model in your own Gemini settings takes
precedence.

Effort uses the same priority order as model selection. You can set both together: a target's
`:model/effort`, `COOP_<AGENT>_MODEL` and a loop.yaml step's `agent:` all accept `model[/effort]`.
For example, `signoff.agent: [claude:opus/xhigh]` uses xhigh effort for final review while you keep
the work step at low effort.

### Which model runs

Each consult peer follows the same model-selection rules and uses its own default when you do
not name a model. `coop loop` shows the model reported by the agent at the start of each iteration.

`coop models` lists the models for each agent. Model IDs change, so co:op passes the ID to the
provider's CLI without checking it against a fixed catalog. The CLI accepts a valid ID or reports
its own error.

## Second opinions

Add read-only peers when you want the lead to get another model's opinion on difficult or risky
decisions. The lead considers their answers and makes all changes itself. Peers work with
interactive runs, loops, forks, ACP and preset consult roles:

```bash
coop codex --peer claude --peer gemini
coop loop claude --peer codex --peer gemini
coop acp claude --peer codex
```

Add each peer with `--peer <target>`, repeating the flag for more:
`coop claude --peer codex --peer gemini`. This also works with a `codex`, `gemini` or `grok` lead.
The lead can use `coop-consult` to ask several peers in parallel. They give read-only advice;
the lead decides what to do.

A peer may use the lead's provider with another model, for example
`coop claude:opus@work --peer claude:haiku`. Its model selection is independent, and it shares the
lead's selected account. Other providers use their default account; `--peer` does not accept
`@account` because one box selects one account per provider.

Asking is optional and off by default. The lead decides whether it needs a second opinion and how
to use the answer; routine work does not require one. Consultations default to `--fresh`, so each
call gets an independent second opinion. Only the peers you name are available. co:op mounts the
selected providers' repository homes writable, while reusable credentials stay outside the box.
The run's broker authenticates their requests.

Peer consultations need no extra service or protocol. Coop mounts a small helper and gives
consultation instructions only to the lead. A peer reads its normal instructions without being
told to consult more peers. Each consultation starts another read-only model run, so include
that usage when choosing how many peers to add.

### Inside the box

Call a preset's consult role by its role name. Several roles can use the same provider while
keeping separate models and instructions. Inside the box, use:

```bash
coop-consult claude --fresh    "<prompt>"   # new read-only session; never edits
coop-consult gemini --continue "<prompt>"   # resume the peer's thread; send only the delta
```

Peers are read-only advisors: they analyze and report, and the leader makes every change itself. A
peer has none of the leader's conversation. So the leader writes a self-contained prompt instead of
forwarding your message verbatim. A follow-up like "fix the second one" means nothing to a peer
that never saw the thread.

### Continuity and fallback

With `coop-consult`, use `--fresh` to start a new consultation or `--continue` to follow up on the
peer's previous one. A follow-up needs only the new information. If Coop can recover the earlier
conversation from a saved transcript, it tells the lead to send just that new information. If it
must start without prior context, the lead needs to supply the full background. A new session can
be resumed only after it has returned a usable reply.

If resuming fails, co:op reports the failure without retrying in the same call. It clears the
unreliable session ID and keeps the last complete transcript. The next `--continue` starts a new
session with the target that last succeeded, using the saved transcript and your new information.
Coop does not reuse the failed ID or quietly make a second paid call.

Coop tries the next fallback target after a confirmed rate limit, a permanent failure, or an
ordinary failure that happens again on one retry. A target has failed permanently if it cannot
start, has invalid configuration or cannot sign in; Coop skips it for the rest of the run.
A timeout or too much output ends the call with an error and does not trigger another paid
attempt on a fallback target. Follow-up consultations use the target that last succeeded.

`coop-consult` manages session IDs for you. It generates IDs for Claude and Gemini, and reads
Codex's ID from its JSON output.

### Limits

Coop captures the provider's standard output and diagnostic output separately. It shows stderr
as diagnostics and never saves or treats it as the peer's answer.

| What | Cap |
| --- | --- |
| Native reply and diagnostic streams | unlimited by default; `COOP_CONSULT_STREAM_LIMIT` sets a limit in bytes |
| Consult time | unlimited by default; `COOP_CONSULT_TIMEOUT` sets a limit in whole seconds |
| stdin | 512 KiB |
| Constructed prompts | 512 KiB |
| Saved transcripts | 512 KiB |

If output exceeds a limit, Coop does not try a fallback target or save a partial reply. If a
complete reply is too large for the saved transcript, Coop returns it but cannot resume that
conversation; use `--fresh` for the next call. After each usable reply, Coop replaces the saved
conversation record in one atomic operation and records its format version.

Only one consultation can use a target at a time. A second call waits for the first to release
its lock. In co:op's image, the operating system releases the lock even if the process crashes.
A custom image without `flock` uses a `mkdir` lock instead and refuses access when it cannot
safely acquire that lock.

### Token usage in loops

Inside `coop loop`, Coop records each provider's consultation usage in the active run's private,
Git-ignored `.agent/runs/<run>.peers.jsonl`. Each successful consultation adds at most one
size-limited row with the token counts and cost reported by the provider. Failed consultations
add nothing, and Coop removes an empty file.

## The orchestrator pattern

Use your strongest model to plan the work, break it into tasks and combine the results. Subagents
use the models you assign to them, so routine implementation can run on cheaper models. Peers
from other providers give independent opinions. This uses co:op's built-in tools:

```bash
coop claude:claude-fable-5-1 --peer codex --peer gemini # run it; --peer mounts the named peers
```

Save the lead model and roles in a [preset](#presets) to reuse this setup. Start it with
`coop <name>` or `coop loop <name>`.

In `.claude/agents/`, assign a larger model to reasoning work and a cheaper model to routine
implementation. These are native Claude Code subagents. The lead chooses them from their
descriptions, and each turn is billed at the subagent's model. Their separate conversations keep
the lead's context smaller. `coop init` does not create these roles. Write your own repository
roles, or use a preset, which generates its own `coop-<role>` inside the box.

With `--peer <target>...`, the lead can ask the named peers (for example codex and gemini)
read-only through `coop-consult <peer>`. They bring different training and different blind spots.
The `--peer` flag declares which peers the lead can consult. A plain `coop claude` does not
enable peer consultations.

For high-stakes decisions, ask a native subagent and a peer to examine the same problem in
parallel. Keep their answers separate until both finish, then compare them and decide.

Use `coop loop claude:claude-fable-5-1 --peer codex --peer gemini` for the same setup in an
unattended loop. Each iteration gets the subagents defined in the repo and the peers named by
`--peer`. This also works in a fork loop:
`coop fork <name> claude --loop --peer codex --peer gemini`.

Inside the box, use `coop-consult` for cross-provider advice. It needs no vendor plugin, keeps
peers read-only so there is one writer per working tree, and limits provider access to the
accounts selected for the run.

## Presets

A preset saves the model and peer choices you would otherwise pass with `:model` and
`--peer <target>`. Store it under `.agent/presets/<name>/`. It names the lead, the roles it can
call and when to use each role. Each role uses one of three modes:

| Mode | What the role is |
| --- | --- |
| `native` | a subagent running inside the lead's own session |
| `consult` | a read-only advisor called with `coop-consult` |
| `delegate` | a worker that can edit files, called with `coop-delegate` |

An example recipe:

```yaml
lead:
  # agent: is a TARGET, or a fallback LADDER (model-first, even cross-provider). A bare
  # provider:model runs on EVERY signed-in account (rotating on rate limit); @account pins
  # one. On a loop it rotates top-to-bottom (running each rung's agent); a single run uses
  # the first. models:/model:/credentials: are retired — the model+account ride agent:.
  agent: [claude:claude-opus-5-5/xhigh, codex:gpt-6-astra/xhigh]
  prompt: roles/lead.md           # optional Markdown, appended to the generated contract

roles:
  thinker:                              # deep thinking + review, read-only
    mode: consult                       # native would run it in the lead's session: every lead must be claude
    agent: claude:claude-fable-5-1/max  # model + effort ride agent:
    when: [architecture, debugging, code-review, before-commit]
    prompt: roles/thinker.md            # the persona it answers as

  critic:                          # independent critique from another vendor, read-only
    mode: consult
    agent: [codex:gpt-6-astra/xhigh, grok:grok-4.5/high]
    when: [plan-review, security, tradeoffs]

  fast:                           # cheap mechanical work, write-capable
    mode: delegate
    agent: [gemini:gemini-3.8-flash, codex:gpt-6-luna]
    when: [boilerplate, bulk-edits, test-scaffolding, repo-survey]
    commit: never                 # it edits; the LEAD reviews the diff, gates, commits
    concurrent: never             # delegate runs are serialized
```

### Run a preset

Use the preset name where the command expects a target, and co:op runs its lead. A
target in that position (`<agent>[:model][/effort][@account]`) runs the agent directly:

```bash
coop presets init                # scaffold the recipe + starter prompt files, ready to edit
coop frontier                    # interactive lead with the full routing contract
coop loop frontier               # unattended: lead credentials rotate, roles ride along
coop acp frontier                # the same preset from an editor
```

An interactive terminal run uses the first lead target in the preset. ACP keeps the full
fallback list and can switch providers or accounts when the current target reaches a rate limit.

### Where presets live

Coop looks for presets in these locations, using the repository copy first:

1. the repo's `.agent/presets/<name>/`
2. the per-user global directory `~/.config/coop/presets/`, which `COOP_PRESETS_DIR` overrides

A global preset such as `frontier` works across repositories without symlinks. If a repository
has a preset with the same name, Coop uses that copy alone; it does not merge the two. `coop presets`
marks presets from the global directory as `(global)`. `coop presets init` creates presets in the
repository. To create a global preset, write it in the global directory yourself.

### What co:op generates

Coop reads the YAML and tells the lead which roles are available, when to use them and how to
call them. Depending on the role, that means the `coop-thinker` subagent,
`coop-consult critic --fresh "…"`, or a `coop-delegate fast <<'EOF' … EOF` heredoc. It also mounts
the required helper scripts. If Coop cannot create any required instruction file, helper or role
prompt, it stops before starting the provider.

A native role runs inside the lead's session, so every lead listed in the preset must use that
role's provider. If they do not match, Coop stops before launch and tells you which role to change.
It generates native subagents for Claude, Codex, Gemini and Grok in each client's own format.
The generated `coop-<role>` uses the role's model, `when` rules and prompt. It exists only in the
box, and `.gitignore` keeps these generated files out of commits. Set `subagent: <name>` to use
a subagent the lead's client already has.

`coop presets init` creates starter prompts in `roles/lead.md`, `roles/thinker.md`,
`roles/critic.md` and `roles/fast.md`. They contain usable defaults that you can edit or delete.
Their text is added to Coop's generated instructions; it cannot replace the safety rules or the
rules for calling roles.

### Fallback ladders

Consult and delegate roles accept one target or a fallback list. Providers without credentials
are skipped. Each available provider's repository home is mounted in the lead's box, and the
run's broker handles authentication. Reusable credentials stay outside the box.

A consult role tries its fallback targets under the rules in
[Continuity and fallback](#continuity-and-fallback). Each role's prompt gives the peer its own
instructions, so two roles using the same provider can do different jobs. Delegates use the
stricter fallback rules below.

### Delegates

Use `coop-delegate` for a worker that can edit files, and `coop-consult` for read-only advice.
Delegates share the working tree and run one at a time. They must never commit. Coop checks
`HEAD`, Git refs and reflogs before and after each attempt. If a delegate changes Git history,
Coop reports an error and keeps the evidence.

Coop tries the next delegate target only after a confirmed rate limit, a failure to start or a
refused login. It skips that target for the rest of the run. Before trying another, Coop checks
that the failed attempt changed no Git history or files, including tracked, untracked, staged
and ignored files. The only exception is Coop's own run record in `.agent/runs`.

Coop limits the prompt size, output size and duration of each delegate attempt. It stops the
provider's process group before allowing another delegate to start. The standard image includes
`flock`, `setsid` and `timeout` for this. A custom image missing these tools refuses to run the
delegate.

A delegate cannot start another delegate. Coop gives the child `COOP_DELEGATE_DEPTH=1`, and any
nested `coop-delegate` call stops before reading input, acquiring a lock or starting a provider.
The child can still ask a configured `coop-consult` for read-only advice.

The lead reviews `git diff`, runs the configured checks, fixes or reverts unsatisfactory changes
and makes the commit. If Coop refuses a delegate attempt, follow the diagnostic's inspection
commands. These are usually `git status --short`, `git diff` and `git diff --cached`, with
`git log` and `git reflog` when Git history changed.

## Instructions

`coop init` sets up shared instructions for all agents. It links `CLAUDE.md` and `GEMINI.md` to
one `AGENTS.md` and gives every agent the same project skills source. Existing instruction files
that are regular files rather than symlinks are left unchanged.

### Instructions that follow you

Put guidance for all your repositories in `~/.config/coop/agents/INSTRUCTIONS.md`. Keep
project-specific rules in that repo's `AGENTS.md`. Inside the box, co:op makes the shared file
available at each agent's global instruction path:

| Agent | Global instruction path |
| --- | --- |
| Claude | `~/.claude/CLAUDE.md` |
| Codex | `~/.codex/AGENTS.md` |
| Gemini | `~/.gemini/GEMINI.md` |
| Grok | `~/.grok/AGENTS.md` |

If the selected repository's provider folder already has its own instruction file, Coop uses it
instead of the shared file. Defaults from `profiles/<account>/` are copied only when a provider
folder is created; editing them does not replace instructions in an existing folder. Edit the
repository's provider-specific file for a local change, or the shared `INSTRUCTIONS.md` for
guidance to use across repositories that have no local override.

### Skills and the .agent/ folder

`coop init` creates the agent directories (`.claude/`, `.codex/`, `.gemini/`) only for agents
you are signed in to. You can choose them explicitly with `--agents claude,codex`, or use `all`.

You can delete an agent's directory from a repo that does not use it. If you later run that
agent in a box, co:op copies the workflow skills from `.agent/skills` into `~/.<agent>/skills`
inside the box. The copy is writable, but the host source stays unchanged. You need the
repository directory again only if you run the agent's own CLI directly on the host.

When the repository has an agent directory, co:op uses that committed copy as it is, and the
agent can edit it. If the repo already has a real `.claude/skills` directory but no `.agent/skills`,
init keeps the existing directory as the skills source. Links for new agents and skills copied
into boxes use that source without adding co:op's templates. Existing valid skills links stay
unchanged.

### Claude settings and hooks

`coop init` keeps fallback Claude settings in `.agent/claude/settings.json` and hooks in
`.agent/claude/hooks/`. For each run, a Claude box copies whichever of these is missing into
`~/.claude`. Existing project `.claude/settings.json` and `.claude/hooks/` take precedence
independently, so having one does not disable the fallback for the other. These temporary copies
never change the host's `.agent/` source.
