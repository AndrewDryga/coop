# Forks

A fork is a temporary local clone of your repo where an agent can work. You review its changes
and decide whether to merge them. The [forks guide](https://coop.dryga.com/docs.html#forks) has a
short overview of the workflow.

An ordinary fork keeps the agent's immediate edits out of your checkout, but you still need to
trust what it writes. Git can share the underlying object files between the clone and your repo.
Git or editor settings written by the agent can also run code when you open the fork on your
machine. Choose [`--isolated`](#isolated-write-forks) for independent Git storage and checks before
changes enter your checkout.

Several forks can run at once. A fork's `origin` points to a local path, not your upstream server.
Uncommitted, Git-ignored secrets do not come along. Secrets already in Git history can still be
present in the clone.

Open a fork, let the agent work, review its changes, then merge them:

```bash
coop fork perf codex     # open: clone into ../<repo>-forks/perf, run codex there
coop fork perf           # re-enter: continues codex's last session by default
coop fork ls             # list your forks (branch, changes, state, last activity)
coop fork review perf    # review: the dossier, then the diff
coop fork merge perf     # land: rebase onto your branch, then close the fork
coop fork rm perf        # or discard it (confirms first; inspect task/Git impact before --force)
```

`coop fork <name>` creates a fork or reopens an existing one. Add an agent, as in
`coop fork perf codex`, or a target such as `codex:gpt-6-astra@work` to choose the model and
account too. You can also use a preset (`coop fork perf frontier`); its lead runs the fork.

A fork inherits your Git identity, signing-key setting and global gitignore from the parent.
The agent's commits use your identity, and Git ignores the same files as in your repo.

Point a fork at a task queue and it works the queue unattended:

```bash
coop fork api codex --loop -d   # loops the repo's task queue(s); -d detaches — tail with coop fork logs api -f
```

[The loop](loop.md) explains how iterations work. [Parallel forks](loop.md#parallel-forks) shows how to run several at once.

## Isolated-write forks

```bash
coop fork research codex --isolated
coop fork research                 # the host remembers isolation
coop fork review research --stat
coop fork merge research --yes
```

An isolated fork keeps its own Git metadata and object files. Coop prevents the box from mounting
your parent checkout or its Git storage, including through another path, a service or an inspected
volume. The agent can still create commits and branches and run rebase inside the fork.

Isolation stays enabled when you reopen the fork, run a detached loop or connect through ACP.
You must create a new isolated fork; you cannot convert an existing ordinary fork. Ordinary forks
and runs that write directly to your checkout keep their existing behavior.

Coop recreates each initialized, unchanged local submodule from its exact commit, with independent
storage. It never copies the submodule's raw Git metadata. If the recorded submodule commits
(gitlinks) or `.gitmodules` have changed, Coop refuses the fork; those changes need a separate
workflow. It copies Git LFS files from Git's default cache in the common directory and verifies
them without network access. Missing or corrupt files and custom `lfs.storage` are refused.

Isolation does not make shared storage private. Other processes can still change homes, caches
or service data that you choose to share with the fork. Use private storage or read-only sources
if you do not trust those processes.

Coop must be able to inspect files in mounts you add, service mounts and existing volumes. If a
path exists only on the Docker daemon's machine, or Coop cannot read it on the host, Coop cannot
treat it as empty. Use a separate bind mount whose contents it can inspect. Coop's own private
volumes are exempt only when Coop can identify them exactly.

Enabled dependency and toolchain caches are also shared storage that you choose to trust. Coop
checks their exact local volume-driver configuration, but cannot promise that unreadable daemon
contents are safe or independent. It refuses custom cache drivers and caches backed by bind
mounts. This cache exception does not apply to other extra volumes or service volumes.

When you review an isolated fork, Coop copies its committed history into private storage and
rebases the copy onto the parent commit it recorded. This leaves both workspaces and the parent's
Git refs unchanged. You can review even while your parent checkout has uncommitted changes.

Before merging, Coop requires a clean checkout on a branch and checks the exact Git index and
tracked-file contents. Hidden index flags or an `index.lock` it cannot account for stop the merge.
Incoming files cannot overwrite untracked or ignored files in your checkout. Coop never merges
untracked files from the fork, including Git metadata for a new child repository.

If the rebase fails, Coop prints the path of the fork it kept and a commit that must remain
unchanged. Keep that commit and its earlier history intact. Fix only the later commits, then
review again. If signing failed, check your trusted signing settings first. Do not run Git's
printed recovery commands in your fork: those commands refer to the temporary clone that Coop
has already discarded.

Coop inspects every incoming commit, including changes that disappear from the final diff. It
refuses unsafe paths or file modes, changes to nested Git sources, files that can automatically
run code on your host, new or changed npm install lifecycle scripts, possible credentials and
oversized files. `--force` cannot bypass these checks. Build files that run only when explicitly
invoked can still be reviewed and merged. Configured checks run on a separate disposable clone
that has the same restrictions on mounting the parent checkout.

`review --open` and `COOP_REVIEW_CMD` show a private copy of the committed files. Coop keeps that
preview for your review tool and prints its path so you can remove it when you finish. A custom
review command also gets a separate private copy of the recorded parent commit as its working
directory. This keeps `HEAD...$COOP_REVIEW_REF` usable for comparisons, while `$COOP_FORK_PATH`
points to the proposed files. Coop prints both paths for cleanup. Edits to a preview are never
included in the merge.

Use only an editor, difftool or review command you trust to run on your host. `coop fork open`
opens the agent's actual workspace instead of a private review copy. Coop warns that Git or
editor metadata in that workspace may run code on your machine.

Cancelling or refusing a merge before it starts leaves the parent's files, index and refs
unchanged. Once Coop starts changing the checkout, it records each step in a journal. These
changes do not happen all at once, so an interruption can leave the checkout partly updated.

Keep the exact reviewed changes and journal, then rerun `coop fork merge <name>`. Coop can resume
only if the checkout exactly matches its original clean state or the complete published state
recorded for that merge. Unrelated edits, a partly updated checkout, a changed branch or an
`index.lock` it cannot account for stop recovery so you can resolve the mismatch. Never reset,
stash, delete an uncertain lock or remove and recreate the fork to bypass this check.

If you keep the fork after merging, the next merge starts after the original fork commit that
was already merged. Coop tracks that source commit separately from its rebased parent SHA.
A reviewed result with no code changes can still complete its assigned tasks. `merge --all`
skips a fork only when it has no new commits and no active task records to finish. Coop completes
tasks from the exact source it reviewed; a commit trailer alone cannot authorize completion.

## Re-entry resumes the session

A fork remembers the agent it was created with. After `coop fork perf codex`, `coop fork perf` re-enters with codex. It never silently falls back to claude. Pass an agent to switch, and `coop fork ls` shows each fork's agent.

Reopening a fork also resumes the last conversation. For Claude, Gemini and Grok, co:op assigns a
separate session ID to each fork, provider and account. Codex creates its own ID, which co:op
records after the run. Coop saves these IDs in the fork's git-excluded `.coop/` folder and uses
them to resume the exact session for the active account and the fork's directory inside the box.

If the saved ID is missing or the provider no longer has that session, co:op starts a new one.
It does not choose another conversation just because it is the newest in that directory.
Switching providers or accounts starts or resumes that provider and account's own session;
it does not copy a conversation between providers.

Coop allows only one of its interactive Codex processes at a time for each account and working
directory inside the box. A second process stops with an error you can retry, so Coop cannot
record the wrong session ID. Codex processes started outside Coop do not respect this lock.
While Coop sets up a new fork session, do not start another Codex process for the same account
and working directory.

| Flag | What it does |
|---|---|
| `--new` | Starts a new session for the selected provider and account. The fork's files stay. |
| `--fresh` | Recreates the whole fork while keeping its selected provider. It asks before deleting anything. Without an interactive terminal, it requires `--yes`. |

On `coop fork rm` and `--fresh`, `--force` can:

- stop the detached worker
- discard unmerged or dirty Git work
- return the fork's assigned tasks to the project's queue
- discard the exact set of changes prepared for review
- discard pending task proposals from the fork

Blocked tasks and proposals already imported into the project's queue stay there. Check
`coop fork ls` and `coop fork review` before discarding a fork. [Land it](#land-it) explains the
different effect of `--force` on `coop fork merge`.

## Review

`coop fork review <name>` shows a review summary before the patch:

- the commits
- the agent's latest task `log.md`, clearly labeled as the agent's own account of its work
- findings from the same policy scan used by `coop fork merge`
- changed files, starting with config and instructions, then code ordered by amount changed, then tests and docs; each file shows `+N -N`
- whether a check command is configured to run before merging

Then the diff opens in your pager, with no setup needed.

The counts and the diff cover committed fork changes only. If the fork has uncommitted or untracked edits, co:op warns you. Open the fork, commit the work you want, then review again. Uncommitted files are not merged.

These flags change how you review:

| Flag | What it does |
|---|---|
| `--stat` | shows the review summary only and skips the diff |
| `--tool` | opens each changed file in your GUI difftool |
| `--open` | opens your editor; isolated forks use a private committed preview |
| `--gate` | rebases a temporary clone, then runs the parent's configured check command in a box with that clone mounted. Ignored build output can be written there, but changes to HEAD or tracked files fail the check. |

`--gate` reports whether the checks passed, failed or could not run because of a rebase conflict.
Passing checks exit 0. A clean rebase also exits 0 when no check command is configured. Failed
checks or a conflict exit 1. The parent and fork refs, branches and worktrees stay unchanged,
and Coop always removes the temporary clone. You can combine `--gate` with `--stat` or `--tool`.
Do not combine it with `--open`: an editor can return before its review window closes.

To review in your editor's source control panel, open the fork as a folder with `coop fork open <name>`. It opens the fork directory with the first of these that's set:

1. `COOP_EDITOR`, a co:op-only override
2. `git config --global core.editor`, your normal Git editor (co:op reads only the global value)
3. a GUI editor detected on `PATH`: `cursor`, `code`, `zed`, `idea`, `subl`
4. `$VISUAL` or `$EDITOR`

Automatic detection uses the order in step 3. If both `code` and `zed` are installed, it chooses
`code`. Set one of the first two options to choose your editor explicitly:

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

Set `COOP_REVIEW_CMD` to run a TUI or review script. co:op invokes it with `sh -c` and provides
`$COOP_FORK_PATH`, `$COOP_FORK_NAME` and `$COOP_REVIEW_REF` in the environment. For an ordinary
fork, the working directory is the parent repo.

For an isolated fork, the command runs in a private copy of the parent commit. `$COOP_FORK_PATH`
points to a separate copy of the fork's committed changes. Neither path is your parent checkout
or the agent's workspace. Coop prints both paths; remove them after the review tool has finished,
even if it returned control to your terminal earlier.

```bash
export COOP_REVIEW_CMD='cd "$COOP_FORK_PATH" && lazygit'
```

## Land it

`coop fork merge <name>` rebases the fork's commits onto your current branch, then advances the
branch to include them. It creates no merge commit. The rebase runs on your host so it can use
your signing key. If you sign commits (`commit.gpgsign=true`), Coop signs the merged commits with
your key even though the box created them unsigned. You then run `git push` yourself.

Set a check command for merges with `gate: make check` in
[`.agent/project.yaml`](configuration.md#project-settings-agentprojectyaml). Commit that file to
share the command with your team. `COOP_GATE="make check"` overrides it on your machine.
Every merge runs the command in the box against the rebased files and rolls back if it fails.
You still review the changes yourself.

Coop checks for oversized files and changes to files the box hides. This includes secret-looking
filenames and paths matched by a [`.coopignore`](box.md#your-own-paths-coopignore), including one
above the project. It also flags additions, edits, removals and renames of a `.coopignore`, because
these change what future boxes can see.

The scan checks each changed file for credential patterns from AWS, OpenAI, Anthropic, GitHub,
Slack and other providers. It also checks for random-looking values assigned to secret-named
keys. A credential can be detected inside an ordinary file even if its name does not look secret.
Coop refuses to merge while your own working tree has uncommitted changes.

Coop asks before merging, then offers to delete the fork after the merge. In a non-interactive
shell, such as CI or a pipe, you must confirm up front; otherwise the merge refuses to run.

| Flag | What it does |
|---|---|
| `--yes`, `-y` | Confirms landing and removal up front, which a non-interactive shell needs. In an interactive shell, it skips the prompts. |
| `--force` | Ordinary forks only: overrides risky-file policy, not the merge gate. Isolated forks have no policy bypass. |
| `--all` | Merges all forks in sequence. See [parallel forks](loop.md#parallel-forks). |

A fork loop takes work from the selected task queues. The host records which task belongs
to which fork run, with a fixed generation ID, and places only the assigned task in the fork.
It never mounts the whole queue there. The agent works on that task copy, including its state,
artifacts, reviews and final signoff. In each task's original queue, the task stays `in_progress`
while the fork is `reviewing` or `ready`.

After final signoff and signing, co:op records one merge candidate for that fork generation:
the exact HEAD commit, file tree and completed task assignments. Before merging, it checks that
candidate again, rebases it and runs the configured checks. It then updates the parent and
completes only the tasks recorded in that candidate. A journal lets Coop resume these steps after
an interruption. A `Coop-Task` trailer helps with consistency checks and search; it cannot
complete a task on its own.

After a stop or crash, the same fork generation can resume its assigned tasks. `rm`, `--fresh`
and discard refuse to proceed while task ownership is unresolved, unless you use the explicit
destructive option that records how those assignments are handled. `coop tasks watch` shows the
project's task states and every sandbox, including cases where the controller's status is stale
or cannot be determined.
