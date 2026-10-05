# Forks

A fork is a throwaway local clone of your repo that an agent works in instead of your working tree. You review its work like a pull request, and you stay the only one who lands anything. The [forks guide](https://coop.dryga.com/docs.html#forks) shows the flow; this page has the details.

The agent never touches your checkout, so a fork adds a layer of isolation. It's also the unit of parallelism: you can run several at once. A fork's `origin` is a local path, so the agent has nowhere to push. Gitignored secrets were never committed, so they don't come along.

The lifecycle mirrors a contractor's pull request: open, work, review, land.

```bash
coop fork perf codex     # open: clone into ../<repo>-forks/perf, run codex there
coop fork perf           # re-enter: continues codex's last session by default
coop fork ls             # list your forks (branch, changes, state, last activity)
coop fork review perf    # review: the dossier, then the diff
coop fork merge perf     # land: rebase onto your branch, then close the fork
coop fork rm perf        # or discard it (confirms first; inspect task/Git impact before --force)
```

`coop fork <name>` opens a new fork, or re-enters one that exists. Name the agent as a target, like `coop fork perf codex`, or `codex:gpt-6-astra@work` to pick its model and account. You can name a preset in the same slot instead (`coop fork perf frontier`), and the preset's lead runs the fork.

A fork inherits your git identity, signing key and global gitignore from the parent. The agent can commit as you, and it ignores the same noise you do.

Point a fork at a task queue and it works the queue unattended:

```bash
coop fork api codex --loop -d   # loops the repo's task queue(s); -d detaches — tail with coop fork logs api -f
```

[The loop](loop.md) explains how iterations work. [Parallel forks](loop.md#parallel-forks) shows how to run several at once.

## Re-entry resumes the session

A fork remembers the agent it was created with. After `coop fork perf codex`, `coop fork perf` re-enters with codex. It never silently falls back to claude. Pass an agent to switch, and `coop fork ls` shows each fork's agent.

The agent's session history persists too, so re-entry continues the last conversation instead of starting fresh. For Claude, Gemini and Grok, co:op gives each fork, provider and account its own session ID. Codex can't be handed a new ID, so co:op records the native ID that Codex mints after the run. These hints stay in the fork's git-excluded `.coop/` folder. Re-entry resumes that exact session for the active account and the fork directory the container sees.

If that exact hint is missing, or the session no longer exists in the provider's history, re-entry starts fresh. co:op never guesses from the latest conversation in a directory. Switching providers or accounts starts or resumes that target's own native session. co:op doesn't splice one provider's transcript into another here.

co:op allows only one interactive Codex process of its own per account and container workdir at a time. A second one fails with a retryable error rather than risk the wrong native session ID. A Codex process started outside co:op can't join that lock. So while a fresh fork session is being set up, don't run one against the same account and working directory.

| Flag | What it does |
|---|---|
| `--new` | Starts a new session for the selected provider and account. The fork's files stay. |
| `--fresh` | Recreates the whole fork and remembers its selected provider. It confirms before it deletes anything, and needs `--yes` when there's no TTY. |

On `coop fork rm` and `--fresh`, `--force` can:

- stop the detached worker
- discard unmerged or dirty Git work
- return owned canonical assignments to the queue
- discard the reviewed generation candidate
- discard pending fork proposals

Blocked tasks and canonical proposals that were already imported stay canonical. Check `coop fork ls` and `coop fork review` first. [Land it](#land-it) covers what `--force` does on `coop fork merge`.

## Review

`coop fork review <name>` opens with a dossier that maps the risk before you see the patch:

- the commits
- the agent's claim, which is the latest task `log.md`, labeled as the fork's own voice
- policy findings from the same scan `coop fork merge` enforces, so nothing first shows up as a failed merge
- the changed files in risk order (config and instructions, then code by churn, then tests, then docs), each with `+N -N`
- whether a merge gate is configured

Then the diff opens in your pager, with no setup needed.

The counts and the diff cover committed fork changes only. If the fork has uncommitted or untracked edits, co:op warns you. Open the fork, commit the work you want, then review again. Uncommitted files are not merged.

These flags change how you review:

| Flag | What it does |
|---|---|
| `--stat` | shows the dossier only and skips the diff |
| `--tool` | opens each changed file in your GUI difftool |
| `--open` | opens the fork in your editor |
| `--gate` | rebases in a disposable scratch clone, then runs the parent's configured gate in the box with that clone mounted; the clone stays writable for ignored build output, and the gate fails if HEAD or the tracked tree changed |

`--gate` reports green, red or a rebase conflict before you merge. Green exits 0, and so does a clean rebase with no gate configured. Red or a conflict exits 1. The parent and fork refs, branches and worktrees stay untouched, and the scratch clone is always removed. Combine `--gate` with `--stat` or `--tool`. Leave out `--open`, whose editor process may return before the review window closes.

To review in your editor's source control panel, open the fork as a folder with `coop fork open <name>`. It opens the fork directory with the first of these that's set:

1. `COOP_EDITOR`, a co:op-only override
2. `git config --global core.editor`, your normal Git editor (co:op reads only the global value)
3. a GUI editor detected on `PATH`: `cursor`, `code`, `zed`, `idea`, `subl`
4. `$VISUAL` or `$EDITOR`

Detection (step 3) is only a best-effort fallback. With both `code` and `zed` installed, it picks `code`. To choose, set one of the first two:

```bash
git config --global core.editor "zed --wait" # your standard git editor (git commit uses it too)
export COOP_EDITOR="zed"                     # coop-only; overrides core.editor
```

co:op runs the editor command verbatim. With `core.editor = "zed --wait"`, your terminal waits until you close the window; that's what `--wait` is for. If you'd rather the command return at once, set `COOP_EDITOR` without `--wait`, like `COOP_EDITOR=zed`.

`--tool` runs `git difftool`, which opens each changed file in whatever your global `diff.tool` points at. co:op uses only the global `diff.tool` and its command. Register a tool once:

```bash
# VS Code
git config --global diff.tool vscode
git config --global difftool.vscode.cmd 'code --wait --diff "$LOCAL" "$REMOTE"'

# Zed
git config --global diff.tool zed
git config --global difftool.zed.cmd 'zed --wait --diff "$LOCAL" "$REMOTE"'
```

JetBrains, Meld, Beyond Compare, vimdiff and others work the same way. `git difftool --tool-help` lists the tools git already knows.

For full control, set `COOP_REVIEW_CMD`. co:op runs it with `sh -c` from the parent repo, with `$COOP_FORK_PATH`, `$COOP_FORK_NAME` and `$COOP_REVIEW_REF` in the environment. It can launch any tool, such as a TUI or a script:

```bash
export COOP_REVIEW_CMD='cd "$COOP_FORK_PATH" && lazygit'
```

## Land it

`coop fork merge <name>` rebases the fork onto your current branch and fast-forwards. History stays linear, with no merge commits. The rebase runs on the host, where your key lives. If you sign commits (`commit.gpgsign=true`), the landed commits are signed with your key, even though the box committed them unsigned. Then run `git push`, the one step the agents can't do.

Set a merge gate with `gate: make check` in [`.agent/project.yaml`](configuration.md#project-settings-agentprojectyaml), which is committed and shared with your team. `COOP_GATE="make check"` sets one per machine, and it wins. Every merge then re-runs that gate in the box on the rebased tree, and rolls back if it goes red. The gate is the machine check behind your human review.

A policy check also flags secret-looking or oversized files. It scans each changed file's content for real credentials: provider token shapes (AWS, OpenAI, Anthropic, GitHub, Slack and more) and high-entropy values on secret-named keys. A token committed inside an ordinary file is caught even when its filename looks innocent. Merge refuses while your own working tree is dirty.

Merging lands the work and then offers to delete the fork, so it asks first. A non-interactive shell (CI, a pipe) has no one to answer, so merge refuses there instead of landing on the default.

| Flag | What it does |
|---|---|
| `--yes`, `-y` | Confirms landing and removal up front, which a non-interactive shell needs. In an interactive shell, it skips the prompts. |
| `--force` | Overrides the policy check. It bypasses only the risky-file policy: the merge gate must still pass on the rebased tree. |
| `--all` | Lands all forks at once. See [parallel forks](loop.md#parallel-forks). |

A fork loop schedules from the project's canonical queue, but it never mounts that whole queue into its sandbox. The host records an immutable fork generation and the exact task assignment, then projects only that task into the fork. The normal in-box lifecycle, artifacts, reviews and signoff work on that projection. In the canonical queue, the task stays `in_progress` while the fork is `reviewing` or `ready`.

After the final signoff and signing, co:op publishes one candidate for the whole generation. It binds the exact HEAD and tree, and every completed assignment. Merge revalidates that candidate, rebases it and runs the gate. Then it advances the parent and completes only those exact canonical tasks, through a land journal it can replay. A `Coop-Task` trailer is still only a label for consistency checks and search. It carries no authority to complete a task.

After a stop or a crash, the same generation can resume its assignments. `rm`, `--fresh` and discard refuse while task authority is unresolved, unless the explicit destructive path journals what happens to it. `coop tasks watch` shows this canonical lifecycle, every sandbox, and any stale or unknown control evidence.
