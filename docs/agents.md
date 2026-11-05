# Agents, accounts and presets

co:op runs four coding agents, with a choice of accounts and models. You can add peers for second
opinions, use presets and provide instructions for each agent. For short introductions, see
[credentials](https://coop.dryga.com/docs.html#credentials),
[models](https://coop.dryga.com/docs.html#models),
[second opinions](https://coop.dryga.com/docs.html#peers) and
[presets](https://coop.dryga.com/docs.html#presets).

## Where agent settings live

co:op gives each repository and selected account its own provider folder. The box mounts it where
the provider normally keeps its files, so conversation history, saved sessions, settings and
indexes stay together.

| Provider | Home in the box | Settings inside that home |
| --- | --- | --- |
| Claude | `~/.claude` | `settings.json` |
| Codex | `~/.codex` | `config.toml` |
| Gemini | `~/.gemini` | `settings.json` |
| Grok | `~/.grok` | `config.toml` |

Under `~/.config/coop/agents/<agent>/`, co:op keeps these separate folders:

| Path | What it holds |
| --- | --- |
| `credentials/<name>/` | the account's credentials and sign-in records, kept only on the host |
| `native-homes/<name>/<repository-key>/home/` | the provider's history, settings and other files for this repository and account |
| `acp-homes/<repository-key>/home/` | one repository's editor-session files, kept when you switch accounts |
| `profiles/<name>/` | what Coop before v11 kept here. Nothing reads or mounts it, and Coop never deletes it |

co:op sets its own defaults in a new provider folder once. Later changes stay with that
repository, and changing the defaults doesn't overwrite an existing folder. A fork co:op created
shares its parent's history storage, and unrelated repositories keep separate history.

## What each run can see

A box gets provider folders only for its lead, named peers and preset roles. Shell, raw-command
and maintenance boxes get none. Sign-in runs in a separate sign-in home for the selected
provider, with no project mounted.

API keys and OAuth access and refresh tokens stay outside coding boxes. Each run has a broker that
adds the credentials to requests sent to the selected provider, and the host renews expiring
credentials without stopping the run.

All agents using the same provider in one box share its selected account. Put agents you don't
trust with each other's access in separate boxes.

### Blast radius

A compromised agent can read and change the history and settings saved for its current
repository, and it can use the selected account through the run's broker. A hook it adds to those
settings can run again in later boxes for the same repository and account. Keeping repositories
separate doesn't protect those local files from the agent or limit what it can spend with the
provider.

The provider folders co:op mounts don't give the agent access to another repository's provider
files. Open networking still lets it send traffic to any destination. To restrict destinations,
use filtered networking. To turn networking off, use `--egress offline` or `COOP_EGRESS=offline`.
Offline runs receive no provider credentials.

Manage shared accounts on the host with `coop login` and `coop credentials … rm`. Don't use a
provider's own login or logout command inside a coding box. If local authentication files don't
match the account state co:op expects, it stops the next launch and keeps the files so you can
recover them. See [migration and recovery](../MIGRATING.md).

## Authentication

### Sign in

Sign in with `coop login`:

```bash
coop login claude     # interactive login; credentials stay in host-only storage
coop login codex      # device-code login (the box has no browser for an OAuth redirect)
coop login gemini     # hidden API-key prompt; stored outside the mounted Gemini home
coop login grok       # device-code login, like Codex
```

`coop claude login` does the same as `coop login claude`, and `coop claude@work login` signs in
to the `work` account.

### API keys

Supported API keys work with both open and filtered Docker networking. Put a key in the shared
env file, or use the provider's sign-in flow on the host:

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
editor sessions and remote sessions all use the broker, so you can switch between API-key and
subscription accounts across runs without exposing either account's credentials to the box.

The broker needs a private Docker network namespace to keep provider credentials outside the box,
so an online provider run can't use host networking, join another container's network or use
Apple's `container` runtime. co:op also refuses custom provider base URLs and credential types it
doesn't support. See
[provider networking](networking.md#provider-credentials-stay-outside-the-box).

### The env file

`KEY=value` sets a value directly. A bare `KEY` copies the value from the environment of the
process running co:op, and co:op leaves it out if that variable is unset. When a variable appears
more than once, the last assignment or successful import wins.

A login supplied only through the env file appears as the provider's default credential in
`coop credentials`, and it covers only that default account. To add named accounts, save a separate
login for each one. When you run a stored named account, even your default, co:op leaves that
provider's token variables out of the run so they can't override the selected login.

co:op shares non-credential variables, such as `DATABASE_URL` and `COOP_NO_ASDF`, with every box.

### What the box receives

The box receives only the public authentication values the provider's native client needs and a
private connection to the run's broker. Before launch, co:op removes reusable provider keys and
tokens from both the user and project environment. It refuses provider credentials passed through
the runtime's `-e` flag, because that would expose them directly to the box. Sign-in boxes also
receive none of the existing API keys from the shared env file or the host's account store.

### First run

On the first run, co:op answers the provider's setup prompts inside the box: Claude's theme,
folder-trust and bypass warnings, Codex's "trust this directory?", and Gemini's and Grok's folder
trust. You don't need to repeat this setup after signing in. co:op treats the mounted repository
as trusted inside the container, where the box provides the sandbox.

## Several accounts and failover

You can save several accounts for one provider, each with its own name and usage limits.
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

The lead's `agent:` list, called a ladder, sets the order of models and accounts to try. You don't
need to configure a separate account pool:

- With no preset, the loop rotates the agent's default model across every signed-in account.
- A bare model (no `@account`) in a ladder does the same.
- A pinned `model@account` runs just that one account.

co:op tracks limits for each model and account pair, so `claude-opus-5-5@personal` stays usable
while `claude-opus-5-5@work` waits for its limit to reset. A ladder tries its fallbacks in the order
you write them, and each step can move to a cheaper model, another account, or both. For example,
run `coop presets init my-ladder`, then edit that preset's lead:

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

Switching accounts loses no work. Each loop iteration is a fresh run, and the queue and Git carry
the progress.

### Choose the default account

The default account is the login a plain interactive `coop claude` uses. Account names have no
special meaning, so choose names that help you recognize them. Use
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

Save the model in a [preset](#presets) if you use it regularly. Each of its roles chooses its own
model. Credentials select the account and subscription, not the model.

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
model that works on tasks, and `signoff.agent` lets you use a stronger model for the final review.
Choose each fork's target or preset in `coop fork <name> <target|preset> --loop`.

If several settings choose a model, the most specific wins:

1. the target's `:model`
2. the preset ladder's active entry
3. `COOP_<AGENT>_MODEL`
4. a model baked into `COOP_<AGENT>_CMD`
5. the agent CLI's own default

### Reasoning effort

Set reasoning effort on the same target by adding `/effort` after the provider or the optional
model: `coop codex/high`, `coop codex:gpt-6-astra/high`, `coop claude:opus/xhigh`,
`coop loop claude:opus/low`. The levels are `low`, `medium`, `high`, `xhigh` and `max`.

co:op refuses known unsupported model-and-effort combinations before starting. It passes any other
level to the agent's CLI:

| Agent | Where the level goes |
| --- | --- |
| Claude | `--effort` |
| Codex | `model_reasoning_effort` |
| Grok | `--reasoning-effort` |
| Gemini | its thinking setting (`low` or `high` only) |

Haiku 4.5 doesn't support reasoning-effort settings, and co:op refuses the combination before
starting. Use `claude:haiku` without an effort suffix, or a model that supports the setting, such
as `claude:opus/low`.

co:op accepts `low` or `high` for Gemini models it can map to the thinking setting, and checks the
value before launch. If you've explicitly saved a thinking setting for a model in your own Gemini
settings, that setting takes precedence.

Effort uses the same priority order as model selection, and you can set both together: a target's
`:model/effort`, `COOP_<AGENT>_MODEL` and a loop.yaml step's `agent:` all accept `model[/effort]`.
For example, `signoff.agent: [claude:opus/xhigh]` uses xhigh effort for final review while you keep
the work step at low effort.

### Which model runs

Each consult peer follows the same model-selection rules and uses its own default when you don't
name a model. `coop loop` shows the model reported by the agent at the start of each iteration.

`coop models` lists the models for each agent. Model IDs change, so co:op passes the ID to the
provider's CLI without checking it against a fixed catalog, and the CLI reports its own error for
an invalid one.

## Second opinions

Add read-only peers when you want the lead to get another model's opinion on difficult or risky
decisions. The lead decides whether to ask and how to use the answer, and it makes all changes
itself. Peers work with interactive runs, loops, forks, ACP and preset consult roles:

```bash
coop codex --peer claude --peer gemini
coop loop claude --peer codex --peer gemini
coop acp claude --peer codex
```

Add each peer with `--peer <target>`, repeating the flag for more. This also works with a `codex`,
`gemini` or `grok` lead. The lead can use `coop-consult` to ask several peers in parallel.

A peer may use the lead's provider with another model, for example
`coop claude:opus@work --peer claude:haiku`. It shares the lead's selected account, and other
providers use their default account. `--peer` doesn't accept `@account`, because one box selects
one account per provider.

Peers are off by default, and only the peers you name are available. co:op mounts the selected
providers' repository homes writable, and the run's broker authenticates the peers' requests.

Only the lead gets consultation instructions, so a peer isn't told to consult more peers. Each
consultation starts another read-only model run, so include that usage when choosing how many peers
to add.

### Inside the box

Call a preset's consult role by its role name. Several roles can use the same provider while
keeping separate models and instructions. Inside the box, use:

```bash
coop-consult claude --fresh    "<prompt>"   # new read-only session; never edits
coop-consult gemini --continue "<prompt>"   # resume the peer's thread; send only the delta
```

A peer has none of the lead's conversation, so the lead writes a self-contained prompt instead of
forwarding your message word for word.

### Continuity and fallback

Use `--fresh` to start a new consultation or `--continue` to follow up on the peer's last one.
`coop-consult` manages session IDs for you. If co:op can recover the earlier
conversation from a saved transcript, it tells the lead to send just the new information. Otherwise
the lead needs to supply the full background. co:op can resume a new session only after it has
returned a usable reply.

If resuming fails, co:op reports the failure and doesn't reuse the failed ID or make a second paid
call on its own. The next `--continue` starts a new session with the target that last succeeded,
using the last complete transcript and your new information.

co:op tries the next fallback target after a confirmed rate limit, a permanent failure, or an
ordinary failure that happens again on one retry. A target has failed permanently if it can't
start, has invalid configuration or can't sign in, and co:op skips it for the rest of the run. A
timeout or too much output ends the call with an error and doesn't trigger another paid attempt on
a fallback target. Follow-up consultations use the target that last succeeded.

### Limits

co:op shows the provider's stderr as diagnostics, and it never saves stderr or treats it as the
peer's answer.

| What | Cap |
| --- | --- |
| Provider reply and diagnostic streams | unlimited by default. `COOP_CONSULT_STREAM_LIMIT` sets a limit in bytes |
| Consult time | unlimited by default. `COOP_CONSULT_TIMEOUT` sets a limit in whole seconds |
| stdin | 512 KiB |
| Constructed prompts | 512 KiB |
| Saved transcripts | 512 KiB |

If output exceeds a limit, co:op doesn't try a fallback target or save a partial reply. If a
complete reply is too large for the saved transcript, co:op returns it but can't resume that
conversation, so use `--fresh` for the next call.

Only one consultation can use a target at a time, and a second call waits for the first to release
its lock. A custom image without `flock` uses a `mkdir` lock instead and refuses access when it
can't safely acquire that lock.

### Token usage in loops

Inside `coop loop`, co:op records each provider's consultation usage in the active run's private,
Git-ignored `.agent/runs/<run>.peers.jsonl`. Each successful consultation adds at most one row with
the token counts and cost reported by the provider, and failed consultations add nothing.

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
implementation. These are native Claude Code subagents: the lead chooses them from their
descriptions, and each turn is billed at the subagent's model. `coop init` doesn't create these
roles, so write your own repository roles or use a preset, which generates its own `coop-<role>`
inside the box.

With `--peer <target>...`, the lead can ask the named peers read-only through
`coop-consult <peer>`, which needs no vendor plugin, keeps one writer per working tree and limits
provider access to the accounts selected for the run.

For high-stakes decisions, ask a native subagent and a peer to examine the same problem in
parallel. Keep their answers separate until both finish, then compare them and decide.

Use `coop loop claude:claude-fable-5-1 --peer codex --peer gemini` for the same setup in an
unattended loop. Each iteration gets the subagents defined in the repo and the peers named by
`--peer`. This also works in a fork loop:
`coop fork <name> claude --loop --peer codex --peer gemini`.

## Presets

A preset saves the model and peer choices you'd otherwise pass with `:model` and
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
  # the first. The model+account ride agent:.
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

An interactive terminal run uses the first lead target in the preset, while ACP keeps the full
fallback list.

### Where presets live

co:op looks for presets in these locations, using the repository copy first:

1. the repo's `.agent/presets/<name>/`
2. the per-user global directory `~/.config/coop/presets/`, which `COOP_PRESETS_DIR` overrides

A global preset such as `frontier` works across repositories without symlinks. If a repository
has a preset with the same name, co:op uses only that copy and doesn't merge the two. The SOURCE
column of `coop presets` says `project` or `global`. `coop presets init` creates presets in the
repository. To create a global preset, write it in the global directory yourself.

### What co:op generates

co:op reads the YAML and tells the lead which roles are available, when to use them and how to
call them. Depending on the role, that means the `coop-thinker` subagent,
`coop-consult critic --fresh "…"`, or a `coop-delegate fast <<'EOF' … EOF` heredoc. If co:op can't
create any required instruction file, helper or role prompt, it stops before starting the provider.

A native role runs inside the lead's session, so every lead listed in the preset must use that
role's provider. If they don't match, co:op stops before launch and tells you which role to
change. It generates native subagents for Claude, Codex, Gemini and Grok in each client's own
format. The generated `coop-<role>` uses the role's model, `when` rules and prompt. It exists only
in the box, and `.gitignore` keeps these generated files out of commits. Set `subagent: <name>` to
use a subagent the lead's client already has.

`coop presets init` creates starter prompts in `roles/lead.md`, `roles/thinker.md`,
`roles/critic.md` and `roles/fast.md`, which you can edit or delete. co:op adds their text to its
generated instructions, and that text can't replace the safety rules or the rules for calling
roles.

### Fallback ladders

Consult and delegate roles accept one target or a fallback list. co:op skips providers without
credentials and mounts each available provider's repository home in the lead's box, and the run's
broker handles authentication.

A consult role tries its fallback targets under the rules in
[Continuity and fallback](#continuity-and-fallback). Delegates use the stricter fallback rules
below.

### Delegates

Delegates share the working tree and run one at a time, and they must never commit. co:op checks
`HEAD`, Git refs and reflogs before and after each attempt. If a delegate changes Git history,
co:op reports an error and keeps the evidence.

co:op tries the next delegate target only after a confirmed rate limit, a failure to start or a
refused login, and it skips that target for the rest of the run. Before trying another, it checks
that the failed attempt changed no Git history or files, including tracked, untracked, staged and
ignored files. The only exception is co:op's own run record in `.agent/runs`.

co:op limits the prompt size, output size and duration of each delegate attempt. The standard image
includes the `flock`, `setsid` and `timeout` this needs, and a custom image missing them refuses to
run the delegate.

A delegate can't start another delegate, but it can still ask a configured `coop-consult` for
read-only advice.

The lead reviews `git diff`, runs the configured checks, fixes or reverts changes it doesn't
accept, and makes the commit. If co:op refuses a delegate attempt, inspect the result with
`git status --short`, `git diff` and `git diff --cached`, plus `git log` and `git reflog` if history
changed.

## Instructions

`coop init` sets up shared instructions for all agents. It links `CLAUDE.md` and `GEMINI.md` to
one `AGENTS.md` and gives every agent the same project skills source. An existing instruction file
stays unchanged if it's a regular file, not a symlink.

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

If the selected repository's provider folder already has its own instruction file, co:op uses it
instead of the shared file. Edit that file for a local change, or the shared `INSTRUCTIONS.md` for
repositories with no local override.

### Skills and the .agent/ folder

`coop init` creates the agent directories (`.claude/`, `.codex/`, `.gemini/`, `.grok/`) only for
agents you're signed in to. You can choose them explicitly with `--agents claude,codex`, or use
`all`.

You can delete an agent's directory from a repo that doesn't use it. If you later run that
agent in a box, co:op copies the workflow skills from `.agent/skills` into `~/.<agent>/skills`
inside the box. The copy is writable, but the host source stays unchanged. You need the
repository directory again only if you run the agent's own CLI directly on the host.

When the repository has an agent directory, co:op uses that committed copy as it is, and the
agent can edit it. If the repo already has a real `.claude/skills` directory but no `.agent/skills`,
`coop init` keeps that directory as the skills source and adds none of co:op's templates. Existing
valid skills links stay unchanged.

### Claude settings and hooks

`coop init` keeps fallback Claude settings in `.agent/claude/settings.json` and hooks in
`.agent/claude/hooks/`. For each run, a Claude box copies whichever of these is missing into
`~/.claude`. Existing project `.claude/settings.json` and `.claude/hooks/` each take precedence on
their own, so having one doesn't disable the fallback for the other. These temporary copies
never change the host's `.agent/` source.
