# Agents, accounts and presets

This page covers the four agents co:op runs, how they sign in, and how you pick their accounts and
models. It also covers second opinions, presets and the instructions every agent reads. The guide
has short overviews of [credentials](https://coop.dryga.com/docs.html#credentials),
[models](https://coop.dryga.com/docs.html#models),
[second opinions](https://coop.dryga.com/docs.html#peers) and
[presets](https://coop.dryga.com/docs.html#presets).

## Where agent settings live

Each provider has a complete native home for each repository and selected account. It is mounted
at the provider's usual location, so native history, resume, settings and indexes stay together.

| Provider | Home in the box | Settings inside that home |
| --- | --- | --- |
| Claude | `~/.claude` | `settings.json` |
| Codex | `~/.codex` | `config.toml` |
| Gemini | `~/.gemini` | `settings.json` |
| Grok | `~/.grok` | `config.toml` |

Under `~/.config/coop/agents/<agent>/`, co:op keeps these separate stores:

| Path | What it holds |
| --- | --- |
| `credentials/<name>/` | canonical host-only account authority |
| `native-homes/<name>/<repository-key>/home/` | one repository's native state for that account |
| `acp-homes/<repository-key>/home/` | editor state for one repository, independent of account switches |
| `profiles/<name>/` | retained older history and host defaults, never mounted as a coding home |

Reviewed host defaults seed a new home once. Later native settings edits stay in that repository;
changing the defaults does not overwrite an existing home. Validated co:op forks share their parent
repository's history domain. Unrelated repositories do not.

## What each run can see

The lead, named peers and preset roles receive only their selected providers' repository homes.
Raw shell/run and maintenance boxes receive no provider homes. Sign-in uses a separate staging
home for only the selected provider, without mounting a project.

Reusable API keys and OAuth access/refresh grants stay outside coding boxes. A run-local broker
authenticates the selected provider requests and the host renews expiring credentials while the
run continues. The box holds public native auth selectors, not the reusable grants. All teammates
of a provider in one box share its selected account's access; separate boxes are the boundary
between mutually untrusted agents.

### Blast radius

A compromised agent can read and edit its current repository's native history and settings, and
can use the selected account through that run's broker. A settings hook can persist into later
boxes for this repository/account. Repository isolation does not make that local history or
configuration untrusted-code-proof, nor does it limit the provider bill by itself.

It cannot read another repository's native home through the managed provider mounts. Open
networking still allows arbitrary outbound traffic; use filtered networking for approved
destinations or `COOP_EGRESS=none` for no network. Offline runs receive no provider grants.

Manage shared accounts with host `coop login` and `coop credentials … rm`, not a native
in-box login/logout command. If local auth files diverge, the next launch refuses and preserves
them for recovery instead of overwriting them. See [migration and recovery](../MIGRATING.md).

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

co:op makes the credential root private (`0700`) and secret files private (`0600`).

| Provider | Supported key | Refused alternatives |
| --- | --- | --- |
| Claude | `ANTHROPIC_API_KEY` | `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN` |
| Codex | `OPENAI_API_KEY` | `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN` |
| Gemini | `GEMINI_API_KEY` | Vertex `GOOGLE_API_KEY` |
| Grok | none | `XAI_API_KEY` |

The same host-only boundary covers native subscription/OAuth accounts. Loops, peers, presets,
editor sessions and remote sessions use the broker too. API-key and subscription accounts can
alternate between runs without granting direct access to their credentials. Offline runs strip
provider keys and do not start a broker.

The broker requires Docker's private network namespace; host networking, joining an arbitrary
container's network and Apple's `container` runtime are not supported for online provider runs.
Custom provider base URLs and unsupported credential families are refused. Native restricted modes
remain Claude-only; remote restricted jobs still do not support filtered networking.
See [provider networking](networking.md#provider-credentials-stay-outside-the-box).

### The env file

`KEY=value` stores a value in the file. A bare `KEY` imports its value when it exists in co:op's
ambient environment. An unset bare import is omitted. A set import or an assignment wins over
earlier entries.

An env-only login appears as the provider's default credential in `coop credentials`, with no
credential marker file needed. It represents only that one default credential. Additional named
accounts need their own account-scoped stored login. When a named account runs, co:op removes that
provider's env-token keys so they can't override the selected account's credential. That holds even
when the stored account is marked as the default.

Any non-credential variable in the file (a `DATABASE_URL`, `COOP_NO_ASDF`, …) is a shared runtime
var and reaches every box.

### What the box receives

A supported brokered run sees public provider-native auth selectors and the run's private broker entrance. co:op
filters the reusable key and every other provider token out of both the user and the project box
environment before launch. Passing a provider credential through runtime `-e` is refused, because
it would bypass that boundary. Sign-in boxes likewise receive none of the existing API keys from the
shared env file or canonical account store.

### First run

On first run, the box pre-answers each agent's setup prompts: Claude's theme, folder-trust and
bypass warnings, Codex's "trust this directory?", and Gemini's folder trust. A fresh install goes
straight from login to work. The box is the sandbox, so trusting the one mounted repo is the
intended posture.

## Several accounts and failover

One agent can hold several accounts as named credentials. Each is a stored login with its own
rate-limit pool, so a long unattended run can ride through a subscription's cap instead of waiting
on it.

```bash
coop login claude@work        # a second account…
coop login claude@personal    # …and a third
coop credentials              # list them and which are signed in
coop usage                    # limits and a 30-day API estimate
coop usage codex@work         # include history and pricing details for one account
```

### Failover

When `coop loop` (or a `coop fork --loop`) hits a rate or usage limit, it switches to the next
target and keeps going. It waits only once every target is limited. A
[Zed (ACP) session](integrations.md) does the same transparently: it rotates, re-sends your prompt
and moves the toolbar dropdown. When every account is cooling down, it waits for the nearest reset.

You don't configure a persistent pool. The rotation is the model-first `agent:` ladder of the
loop's lead:

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

You choose which credential a plain interactive `coop claude` uses by marking it. No name is
special, so you can name every account meaningfully. Mark one with
`coop credentials <agent> <credential> default`, and remove one with
`coop credentials <agent> <credential> rm`:

```bash
coop credentials claude personal default  # `coop claude` now runs on the personal account
```

## Picking models

Every launch names its model in the target, `<agent>:<model>`. You pick the model per run, on any
path:

```bash
coop claude:opus                                    # one big-model interactive session
coop claude:fable --peer codex --peer gemini        # the lead's model (peers keep their own)
coop loop claude:haiku                              # a cheap overnight grind
coop fork risky claude:opus                         # a careful fork on the big model
coop acp claude:sonnet                              # pin an editor entry's model
```

For a standing model you don't retype, put it in a [preset](#presets). The lead's `agent:` ladder
is the model, and on a loop it's also the rotation across your accounts. Each role names its own. A
credential is just an account (which subscription). The model is a separate axis:

```bash
coop models                        # the model menu per agent
coop claude:opus                   # one run on the big model
coop codex/high                    # default model at high reasoning effort
coop claude:opus/xhigh             # …at extra-high reasoning effort (low·medium·high·xhigh·max)
coop frontier                      # a standing lead model + roles, from the preset (its lead leads)
```

One env knob rounds it out. [`COOP_<AGENT>_MODEL`](configuration.md#environment-variables) is the
agent-wide default, for example `COOP_CLAUDE_MODEL=fable`.

For the loop, put the per-step model in [`.agent/loop.yaml`](loop.md). `work.agent` sets it for the
iterations. `signoff.agent` sets a stronger final reviewer over the cheaper work loop. Give each
fork its own target or preset in the `coop fork <name> <target|preset> --loop` command.

If more than one is set, the most specific wins:

1. the target's `:model`
2. the preset ladder's active entry
3. `COOP_<AGENT>_MODEL`
4. a model baked into `COOP_<AGENT>_CMD`
5. the agent CLI's own default

### Reasoning effort

Reasoning effort is a sibling axis on the same target. Add `/effort` after the provider or the
optional model: `coop codex/high`, `coop codex:gpt-6-astra/high`, `coop claude:opus/xhigh`,
`coop loop claude:opus/low`. The levels are `low`, `medium`, `high`, `xhigh` and `max`.

co:op passes the level to the agent's CLI, except for known unsupported model-and-effort combinations
which it refuses before starting:

| Agent | Where the level goes |
| --- | --- |
| Claude | `--effort` |
| Codex | `model_reasoning_effort` |
| Grok | `--reasoning-effort` |
| Gemini | its thinking setting (`low` or `high` only) |

Haiku 4.5 has no reasoning-effort control. Use `claude:haiku` without an effort suffix, or choose a
supporting model such as `claude:opus/low`. The native CLI can silently drop Haiku's effort, while
its editor adapter rejects the missing option; co:op catches the combination first.

Gemini has no effort flag, so co:op sets its thinking instead and checks the level up front. It
takes `low` or `high`, on the models co:op can map. Gemini 3 thinks at only those two levels, and
any Gemini model can end up handing a turn to one. A thinking setting you pin on one model in your
own Gemini settings still wins.

Effort resolves through the same tiers as the model, and one setting carries both. A target's
`:model/effort`, `COOP_<AGENT>_MODEL` and a loop.yaml step's `agent:` all take `model[/effort]`.
For example, `signoff.agent: [claude:opus/xhigh]` signs off at xhigh while the work loop grinds low.

### Which model runs

Consult peers pick their model the same way: each peer resolves its own default. `coop loop`'s live
view prints the model each iteration actually ran. It comes from the agent's own init report rather
than from co:op, so it's ground truth.

co:op never validates a model id. `coop models` lists each agent's models, and ids churn. Whatever the agent
CLI accepts works, and a bad id fails loudly in the agent's own error.

## Second opinions

Name read-only peers when a lead would benefit from a different model's blind spots. The lead may
consult them on genuinely hard or risky calls, then weighs their answers. It stays the sole writer.
Peers work with interactive runs, loops, forks, ACP and preset consult roles:

```bash
coop codex --peer claude --peer gemini
coop loop claude --peer codex --peer gemini
coop acp claude --peer codex
```

Name each peer with `--peer <target>`, and repeat the flag for more:
`coop claude --peer codex --peer gemini`. A `codex`, `gemini` or `grok` lead works the same way.
The lead may ask its peers read-only and in parallel, through the same `coop-consult` wrapper, then
decide.

A peer may use the lead's provider with another model, for example
`coop claude:opus@work --peer claude:haiku`. Its model selection is independent, and it shares the
lead's selected account. Other providers use their default account; `--peer` does not accept
`@account` because one box selects one account per provider.

Asking is optional and off by default. There's no synthesis mandate, and it isn't meant for routine
work. It defaults to `--fresh`, so each hard call gets an independent second opinion. Only the peers
you name are consulted. co:op never consults everyone signed in, and only a named peer's
credentials are mounted, read-only.

There's no extra service or protocol. co:op mounts a small wrapper and gives the optional
consultation instructions only to the lead you launched. Peers it spawns read their normal
instructions, so they never recurse. Each consultation adds one read-only run, so account for it
when you choose peers.

### Inside the box

Preset consult roles are addressed by role name, so several roles can use one provider with
distinct models and personas. Inside the box the interface is:

```bash
coop-consult claude --fresh    "<prompt>"   # new read-only session; never edits
coop-consult gemini --continue "<prompt>"   # resume the peer's thread; send only the delta
```

Peers are read-only advisors: they analyze and report, and the leader makes every change itself. A
peer has none of the leader's conversation. So the leader writes a self-contained prompt instead of
forwarding your message verbatim. A follow-up like "fix the second one" means nothing to a peer
that never saw the thread.

### Continuity and fallback

`coop-consult` adds optional continuity. `--fresh` starts a new session. `--continue` resumes the
peer's own prior consult, so a follow-up can send just the delta instead of re-pasting context. The
wrapper tells a transcript-backed fresh recovery, where the leader sends only the delta, apart from
a plain fresh start, where the leader must resend full context. A fresh session becomes resumable
only after a usable reply.

A failed resume isn't retried in the same call. co:op returns that failure once, clears the
uncertain native session id and keeps the last complete transcript. The next `--continue` restarts
that same successful rung fresh, with the transcript plus the new delta. It never retries a dead id
or silently double-bills the failed call.

A fallback ladder advances on a proven rate limit, a permanent failure, or an ordinary failure that
repeats on one retry. A permanent failure means the target can't start, is misconfigured, or its
login is refused. That target is then skipped for the rest of the run. A timeout or output overflow
is terminal for that call: it stays visible and never spends a second rung. Consult continuity
follows the successful rung.

`coop-consult` hides the per-agent session-id mechanics. Claude and Gemini start under a generated
id, and Codex's is captured from its JSON stream.

### Limits

Provider stdout and diagnostics are captured separately. stderr is shown as a diagnostic, but it is
never accepted or persisted as the advisor reply.

| What | Cap |
| --- | --- |
| Native reply and diagnostic streams | unlimited by default; `COOP_CONSULT_STREAM_LIMIT` sets a limit in bytes |
| Consult time | unlimited by default; `COOP_CONSULT_TIMEOUT` sets a limit in whole seconds |
| stdin | 512 KiB |
| Constructed prompts | 512 KiB |
| Saved transcripts | 512 KiB |

Overflow never triggers fallback or persists a partial reply. If a valid reply would overflow the
saved transcript, co:op returns the reply and drops continuity, so the next call must use `--fresh`.
Continuation is one versioned record, replaced atomically after a usable reply.

A second consult for the same target waits on a kernel-held lock, which the co:op image releases
even after an unclean exit. A custom image without `flock` uses a fail-closed `mkdir` fallback.

### Token usage in loops

Inside `coop loop`, every provider's consult records its usage. At most one bounded row per
successful consult is appended to the active run's private, ignored `.agent/runs/<run>.peers.jsonl`,
with the token counts and cost the provider reported. An empty file is removed, and a failed consult
appends nothing.

## The orchestrator pattern

Put your strongest model in charge and let it spend the cheap tokens. The lead plans, decomposes
and synthesizes. Pinned subagents execute, and cross-vendor peers give independent opinions. It all
composes from pieces co:op already has, with no plugins:

```bash
coop claude:claude-fable-5-1 --peer codex --peer gemini # run it; --peer mounts the named peers
```

For a standing arrangement (a lead model plus roles you don't retype), put it in a
[preset](#presets) and run `coop <name>` or `coop loop <name>`.

Tier your subagents: pin a reasoning specialist to a big model and a mechanical worker to a cheap
one in `.claude/agents/`. They're native Claude Code subagents. The lead auto-delegates to them on
their descriptions, each turn bills at that subagent's own model, and the lead's context stays lean.
`coop init` scaffolds none. A preset generates its own `coop-<role>` in the box, and your repo's
roles are yours to write.

With `--peer <target>...`, the lead can ask the named peers (for example codex and gemini)
read-only through `coop-consult <peer>`. They bring different training and different blind spots.
The `--peer` flag matters: a plain `coop claude` deliberately doesn't mount peer credentials, so
peers answer only in a consult-capable box.

For high-stakes calls, task a native subagent and a peer on the same problem in parallel. Don't show
either one the other's answer. Then synthesize.

The same arrangement runs unattended. `coop loop claude:claude-fable-5-1 --peer codex --peer gemini`
makes every iteration orchestrate this way. The pinned subagents ride along in the repo, and
`--peer` mounts the named peers into each iteration's box. Fork loops take it too:
`coop fork <name> claude --loop --peer codex --peer gemini`.

In the box, prefer `coop-consult` over vendor cross-agent plugins. There's nothing to install, peers
stay read-only (one writer per tree), and the credential scoping is already handled.

## Presets

The orchestrator pattern above is assembled by hand: a `:model` here, a `--peer <target>` there. A
preset declares the whole arrangement once, as a runtime recipe under `.agent/presets/<name>/`. It
says who leads and which roles the lead routes work to. Each role uses one of three modes:

| Mode | What the role is |
| --- | --- |
| `native` | a subagent inside the lead's own session |
| `consult` | a read-only peer, reached through `coop-consult` |
| `delegate` | a write-capable delegate, reached through `coop-delegate` |

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

Name the preset in the who-runs slot, and its lead leads. A target in that same slot
(`<agent>[:model][/effort][@account]`) runs the agent directly instead:

```bash
coop presets init                # scaffold the recipe + starter prompt files, ready to edit
coop frontier                    # interactive lead with the full routing contract
coop loop frontier               # unattended: lead credentials rotate, roles ride along
coop acp frontier                # the same preset from an editor
```

A terminal preset run uses the first lead target. ACP keeps the complete ladder and can rotate
across providers and accounts when the active rung is rate limited.

### Where presets live

Presets resolve from two locations, and the repo wins:

1. the repo's `.agent/presets/<name>/`
2. the per-user global directory `~/.config/coop/presets/`, which `COOP_PRESETS_DIR` overrides

A global recipe like `frontier` applies across every repo without symlinking. A repo preset shadows
a same-named global one, with no merging. `coop presets` tags a global-sourced preset `(global)`.
`coop presets init` scaffolds into the repo. Author a global preset by hand.

### What co:op generates

co:op generates the lead's routing contract from the YAML: each role, when to use it, and its
role-addressed invocation. That invocation is the `coop-thinker` subagent,
`coop-consult critic --fresh "…"`, or a `coop-delegate fast <<'EOF' … EOF` heredoc. co:op also
mounts the wrappers. The required routing files, wrappers and role prompts are assembled as one
contract. If any of them can't be created, co:op exits before starting the provider instead of
silently dropping a role.

A native role runs inside the lead's own session, so every lead the preset lists must use the
role's provider. Otherwise co:op stops before it starts and names the role to change, rather than
quietly running it as something else. co:op generates the role's subagent in the box, in the lead
client's own subagent format, for Claude, Codex, Gemini and Grok alike. The subagent is
`coop-<role>`, built from the role's model, `when` and prompt. It's never written to your repo, and
`.gitignore` keeps the overlay out of commits. Set `subagent: <name>` to reference one the lead's
client already has instead.

`coop presets init` scaffolds starter `roles/lead.md`, `roles/thinker.md`, `roles/critic.md` and
`roles/fast.md`. They
hold usable defaults. Their Markdown feeds the generated text and never replaces the safety and
routing rules. Edit or delete them freely.

### Fallback ladders

Consult and delegate roles accept one target or a fallback list. Providers without mounted
credentials are skipped. Every available rung's credential home is mounted for the lead's box.

A consult role's wrapper moves down its list under the rules in
[Continuity and fallback](#continuity-and-fallback). The role's prompt, if any, is the persona the
peer adopts, so two consult roles on one agent stay distinct. A delegate falls back under the
stricter rules below.

### Delegates

`coop-delegate` is the write-capable counterpart of the read-only `coop-consult`. The delegate may
edit the shared worktree, and delegate runs are serialized. It must never commit. The wrapper checks
`HEAD`, refs and reflogs before and after each attempt. If history changed, it fails loudly without
discarding the evidence.

A delegate falls back to its next rung only after a proven rate limit, or when the target couldn't
start or its provider refused its login. That target is then skipped for the rest of the run.
Fallback also needs a clean worktree: the failed rung changed no Git history and no files, whether
tracked, untracked, staged or ignored. co:op's own run ledger in `.agent/runs` is the one exception.

Prompt and output are bounded, and each attempt has a timeout. co:op cleans up the provider process
group before it releases the serialization lock. The standard image supplies `flock`, `setsid` and
`timeout`, and a custom image missing them fails closed.

Write-capable delegation is one level deep. The child receives `COOP_DELEGATE_DEPTH=1`, and a nested
`coop-delegate` fails before input, lock or provider launch. A delegate may still call a configured
read-only `coop-consult` for advice.

The lead then reviews `git diff`, runs the gate, fixes or reverts what falls short, and makes the
commit itself. On a refusal, run the commands the diagnostic names. Usually those are
`git status --short`, `git diff` and `git diff --cached`, plus `git log` and `git reflog` for
history violations.

## Instructions

`coop init` wires a tool-neutral setup, so every agent reads the same instructions. `CLAUDE.md` and
`GEMINI.md` symlink to a canonical `AGENTS.md`, and every agent shares one project skills source. A
real (non-symlink) instruction file you already have is left untouched.

### Instructions that follow you

A shared `~/.config/coop/agents/INSTRUCTIONS.md` holds machine-level guidance that follows you
across repos. Project rules stay in the repo's own `AGENTS.md`. co:op wires the shared file into
each agent's global instruction path in the box:

| Agent | Global instruction path |
| --- | --- |
| Claude | `~/.claude/CLAUDE.md` |
| Codex | `~/.codex/AGENTS.md` |
| Gemini | `~/.gemini/GEMINI.md` |
| Grok | `~/.grok/AGENTS.md` |

A provider-native instruction file in the selected repository home wins over the shared file.
Host defaults from `profiles/<account>/` seed new homes only; editing them does not replace an
existing repository home's instructions. Edit that repository's native instruction file when the
change should stay local, or the shared `INSTRUCTIONS.md` for machine-wide fallback guidance.

### Skills and the .agent/ folder

The `.agent/` folder is the normal cornerstone. `coop init` scaffolds the per-agent dirs
(`.claude/`, `.codex/`, `.gemini/`) only for the agents you're signed in to, or for the ones you name
with `--agents claude,codex` (or `all`).

A repo that never uses an agent can delete its dir. When a box runs an agent whose dir is absent,
co:op synthesizes the workflow skills from `.agent/skills` at the box's user-level
`~/.<agent>/skills`. That's a writable copy, and the host source stays untouched. Only running that
agent's own CLI on the host needs the dir back; boxes don't.

When a repo has the per-agent dir, that committed copy wins and is used as is, so a box can still
self-improve it. If the repo already has a real `.claude/skills` and no `.agent/skills`, init keeps
it as the source instead of creating a competing tree. New agent links and box synthesis use it
without adding co:op's templates. Existing valid skills links are left alone.

### Claude settings and hooks

Claude's settings and hooks follow the same rule. `coop init` keeps fallback copies in
`.agent/claude/settings.json` and `.agent/claude/hooks/`. A Claude box copies each missing artifact
to `~/.claude` for that run. Existing project `.claude/settings.json` and `.claude/hooks/` artifacts
win independently. The temporary copies never modify the host's `.agent/` source.
