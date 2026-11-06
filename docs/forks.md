# Forks

A fork is a temporary local clone of your repo where an agent can work. You review its changes
and decide whether to merge them. The [forks guide](https://coop.dryga.com/docs.html#forks) has a
short overview of the workflow.

An ordinary fork keeps the agent's edits out of your checkout while it works, but you still need
to trust what it writes. Git can share object files between the clone and your repo, and Git or
editor settings the agent writes can run code when you open the fork on your machine. Use
[`--isolated`](#isolated-write-forks) to give the fork independent Git storage and to check changes
before they enter your checkout.

Several forks can run at once. A fork's `origin` points to a local path, not your upstream server.
Secrets already in Git history can still be in the clone. At creation, Coop also copies present
`services.require_real_files` and regular files in the same directories that Compose binds, such
as a generated key's certificate. Other ignored files stay out. These are independent copies;
reopening a fork leaves them unchanged, and existing files in the clone take precedence.

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
account too. You can also name a preset, as in `coop fork perf frontier`, and the preset's lead runs
the fork.

A fork inherits your Git identity, signing-key setting and global gitignore from the parent repo.
At creation, Coop also snapshots the parent's network, service, project-build and
`coop approve --case-exposure` file approvals. They keep the same approved inputs and
restrictions; changing the fork's rules, Compose file or build files still needs approval there. A
fork refused for new network access names the command that reviews it,
`COOP_REPO=<fork folder> coop approve`, and one refused for new build files names
`COOP_REPO=<fork folder> coop build --egress filtered`. You can run both from the project. Later
parent approvals do not update an existing fork, and a fork recreated with `--fresh` takes the
parent's approvals again. Coop can build a separate fork image from unchanged approved files without
asking again.
Approved external or explicitly named service volumes still refer to the same Docker volumes.

Coop refuses to copy approvals whose parent identity or stored records are invalid. A network
approval withdrawn at a fork's path stays withdrawn even if you recreate that fork; run
`coop approve` in the fork to grant access again. Removing a fork removes the approvals it holds,
and leaves the parent's alone.

Point a fork at a task queue and it works the queue unattended:

```bash
coop fork api codex --loop -d   # loops the repo's task queue(s); -d detaches — tail with coop fork logs api -f
```

[The loop](loop.md) explains how iterations work. [Parallel forks](loop.md#parallel-forks) shows
how to run several at once.
A refused loop appears as stopped in `coop fork ls`, with the reason beneath it. A fork whose box
must wait for other boxes before it can start its services appears as waiting to start services,
with the boxes it waits for beneath it. When a task needs your decision, it waits in
`coop tasks decisions` while the fork goes on with its other tasks. The fork stops for the decision
only when nothing else is left. Answer it from the project with `coop tasks unblock <id> "<answer>"`
or `coop tasks decisions -i`: the fork picks the task up on its next pass, and a stopped fork
resumes with `coop fork <name> --loop -d`. An answer typed into `decision.md` by hand still needs
`coop tasks unblock <id>` to send the task back, and a stopped fork says which of its tasks have
one. `unblock` also finds a task in a queue the fork was given with `--tasks`, and run in the
fork's own checkout, it answers the task in the project.

A fork loop makes its reviewed work ready to merge only when it has no other task to take. To merge
each task once it's reviewed, add `--stop-after-review`. The fork stops after a task passes review,
before it takes another, and `coop fork ls` shows it as ready to merge. Merge it and let the merge
delete the fork, then run the same command again for the next task:

```bash
coop fork api codex -d --stop-after-review  # stops once a task passes review
coop fork merge api --yes                   # merges the reviewed work and deletes the fork
coop fork api codex -d --stop-after-review  # a new fork takes the next task
```

A fork you keep after merging takes no new task: its files stay for you to look at, and a loop on it
refuses and names how to go on, either `--fresh` to start it over or a new fork. A loop that finds no
task to take, because the queue is empty or other agents hold what is left, says so in its log
(`coop fork logs`) and beside the idle fork in `coop fork ls`.

## Isolated-write forks

```bash
coop fork research codex --isolated
coop fork research                 # the host remembers isolation
coop fork review research --stat
coop fork merge research --yes
```

An isolated fork keeps its own Git metadata and object files. co:op prevents the box from mounting
your parent checkout or its Git storage, including through another path, a service or a volume
that co:op inspects. The agent can still create commits and branches and run rebase inside the
fork.

Isolation stays enabled when you reopen the fork, run a detached loop or connect through ACP. You
can't convert an existing ordinary fork, so you must create a new isolated one. Ordinary forks and
runs that write directly to your checkout don't get this isolation. `coop fork ls` marks an isolated
fork with `Isolated: yes`, and `coop fork ls --json` has an `isolated` field for every fork.

### What an isolated fork copies

co:op recreates each initialized, unchanged local submodule from its exact commit, with independent
storage, and never copies the submodule's raw Git metadata. `coop fork review` and `coop fork merge`
refuse an isolated fork whose commits change a submodule commit or `.gitmodules`, so make those
changes in your own checkout.

co:op copies Git LFS files from Git's default cache in the common directory and verifies them
without network access. It refuses missing or corrupt files and a custom `lfs.storage`.

### Storage you share with the fork

Isolation doesn't make shared storage private. Other processes can still change provider folders,
caches or service data that you choose to share with the fork, so use private storage or read-only
sources if you don't trust those processes.

- co:op must be able to inspect the files in mounts you add, service mounts and existing volumes.
  If a path exists only on the Docker daemon's machine, or co:op can't read it on the host, co:op
  refuses the mount. Use a separate bind mount whose contents co:op can inspect. co:op's own
  private volumes are exempt only when co:op can identify them exactly.
- Dependency and toolchain caches you enable are shared storage you choose to trust. co:op checks
  their exact local volume-driver configuration, but it can't promise that daemon contents it can't
  read are safe or independent. It refuses custom cache drivers and caches backed by bind mounts.
  This cache exception doesn't apply to other extra volumes or service volumes.

### Reviewing an isolated fork

When you review an isolated fork, co:op copies its committed history into private storage and
rebases the copy onto the parent commit it recorded. This leaves your checkout, the fork's
workspace and the parent's Git refs unchanged, so you can review even while your parent checkout
has uncommitted changes.

`review --open` and `COOP_REVIEW_CMD` show a private copy of the committed files. co:op prints its
path and the command that removes it, and deleting the fork removes it too. Edits to a preview are
never included in the merge.

Use only an editor, difftool or review command you trust to run on your host. `coop fork open`
opens the agent's actual workspace instead of a private review copy, and co:op warns that Git or
editor metadata in that workspace may run code on your machine.

### Merging an isolated fork

Before merging, your checkout must be clean and on a branch. co:op also checks the exact Git index
and tracked-file contents, and hidden index flags or an `index.lock` it can't account for stop the
merge. Incoming files can't overwrite untracked or ignored files in your checkout. co:op never
merges untracked files from the fork, including Git metadata for a new child repository.

co:op inspects every incoming commit, including changes that disappear from the final diff. It
refuses the following, and `--force` cannot bypass the refusal:

- unsafe paths or file modes
- changes to nested Git sources
- files that can automatically run code on your host
- new or changed npm install lifecycle scripts
- possible credentials that a commit adds
- oversized files

You can still review and merge build files that run only when someone explicitly invokes them.
Configured checks run on a separate disposable clone, which has the same restrictions on mounting
the parent checkout.

If the rebase fails, co:op prints the path of the fork it kept and a commit that must remain
unchanged. Keep that commit and its earlier history intact, fix only the later commits, then review
again. If signing failed, check your trusted signing settings first. Don't run Git's printed
recovery commands in your fork, because they refer to the temporary clone co:op has already
discarded.

If you keep the fork after merging, the next merge starts after the original fork commit that was
already merged.

A reviewed result with no code changes can still complete its assigned tasks. `merge --all` skips
a fork only when it has no new commits and no active task records to finish. co:op completes tasks
from the exact source it reviewed.

### When a merge is interrupted

If you cancel a merge or co:op refuses it before it starts, the parent's files, index and refs stay
unchanged and nothing is left to resume, so the next merge asks again and runs the checks again.
Once co:op starts changing the checkout, it records each step in a journal, and an interruption can
leave the checkout partly updated.

Keep the exact reviewed changes and the journal, then rerun `coop fork merge <name>`. co:op can
resume only if the checkout exactly matches its original clean state or the complete merged state
recorded for that merge. If it finds unrelated edits, a partly updated checkout, a changed branch or
an `index.lock` it can't account for, recovery stops so you can resolve the mismatch. Never reset,
stash, delete a lock you cannot identify, or remove and recreate the fork to bypass this check.

## Re-entry resumes the session

A fork remembers the target you created it with: its agent, model, effort and account. After
`coop fork perf codex:gpt-6-astra@work`, `coop fork perf` re-enters with that model on that account,
and so does a loop you resume with `coop fork perf --loop -d`. It never falls back to another agent,
model or account on its own. Pass a target to switch, and the fork remembers the new one. If the
saved account has been removed, co:op names the saved target and says how to pick another.
`coop fork ls` shows each fork's agent.

Reopening a fork also resumes the last conversation. co:op saves a session ID for each fork,
provider and account in the fork's git-excluded `.coop/` folder, and uses it to resume the exact
session for the active account and the fork's directory inside the box.

If the saved ID is missing or the provider no longer has that session, co:op starts a new one. It
doesn't choose another conversation just because it's the newest in that directory. Switching
providers or accounts starts or resumes that provider and account's own session, and doesn't copy a
conversation between providers.

co:op allows only one of its interactive Codex processes at a time for each account and working
directory inside the box. A second process stops with an error you can retry, so co:op can't record
the wrong session ID. Codex processes started outside co:op don't respect this lock, so while co:op
sets up a new fork session, don't start another Codex process for the same account and working
directory.

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
- the agent's notes, clearly labeled as its own account of its work: each task's `log.md` under the
  task's title, without the template comment. A task with no notes is left out.
- findings from the same policy scan that `coop fork merge` uses
- changed files, starting with config and instructions, then code ordered by amount changed, then
  tests and docs. Each file shows `+N -N`.
- whether a check command is configured to run before merging

Then the diff opens in your pager.

The counts and the diff cover committed fork changes only. If the fork has uncommitted or untracked
edits, co:op warns you. Open the fork, commit the work you want, then review again. Uncommitted
files are not merged.

These flags change how you review:

| Flag | What it does |
|---|---|
| `--stat` | shows the review summary only and skips the diff |
| `--tool` | opens each changed file in your GUI difftool |
| `--open` | opens your editor. Isolated forks use a private preview of the committed files. |
| `--gate` | rebases a temporary clone, then runs the parent's configured check command in a box with that clone mounted. Ignored build output can be written there, but changes to HEAD or tracked files fail the check. |

`--gate` exits 0 when the checks pass, or after a clean rebase when no check command is configured.
It exits 1 when the checks fail or the rebase conflicts. The parent and fork refs, branches and
worktrees stay unchanged, and co:op always removes the temporary clone. You can combine `--gate`
with `--stat` or `--tool`, but not with `--open`, because an editor can return before its review
window closes.

To review in your editor's source control panel, run `coop fork open <name>`. It opens the fork
directory with the first of these that's set:

1. `COOP_EDITOR`, a co:op-only override
2. `git config --global core.editor`, your normal Git editor (co:op reads only the global value)
3. a GUI editor detected on `PATH`: `cursor`, `code`, `zed`, `idea`, `subl`
4. `$VISUAL` or `$EDITOR`

Automatic detection uses the order in step 3, so if both `code` and `zed` are installed, it chooses
`code`. Set one of the first two options to choose your editor explicitly:

```bash
git config --global core.editor "zed --wait" # your standard git editor (git commit uses it too)
export COOP_EDITOR="zed"                     # coop-only; overrides core.editor
```

co:op runs the editor command as written, so with `core.editor = "zed --wait"` your terminal waits
until you close the window. To return at once, set `COOP_EDITOR` without `--wait`, like
`COOP_EDITOR=zed`.

`--tool` runs `git difftool`, which opens each changed file in your `diff.tool`. co:op uses only the
global `diff.tool` and its command. Register a tool once:

```bash
# VS Code
git config --global diff.tool vscode
git config --global difftool.vscode.cmd 'code --wait --diff "$LOCAL" "$REMOTE"'

# Zed
git config --global diff.tool zed
git config --global difftool.zed.cmd 'zed --wait --diff "$LOCAL" "$REMOTE"'
```

JetBrains, Meld, Beyond Compare, vimdiff and others work the same way.
`git difftool --tool-help` lists the tools git already knows.

Set `COOP_REVIEW_CMD` to run a TUI or review script. co:op runs it with `sh -c` and provides
`$COOP_FORK_PATH`, `$COOP_FORK_NAME` and `$COOP_REVIEW_REF` in the environment. For an ordinary
fork, the working directory is the parent repo. With `--gate`, it is the temporary clone instead,
and co:op removes the clone when the command exits.

For an isolated fork, the working directory is a private copy of the parent commit, which keeps
`HEAD...$COOP_REVIEW_REF` usable for comparisons, and `$COOP_FORK_PATH` points to a separate copy of
the fork's committed changes. Neither path is your parent checkout or the agent's workspace. co:op
prints both paths. Remove them after the review tool has finished, even if it returned control to
your terminal earlier.

```bash
export COOP_REVIEW_CMD='cd "$COOP_FORK_PATH" && lazygit'
```

## Land it

`coop fork merge <name>` rebases the fork's commits onto your current branch, then advances the
branch to include them, with no merge commit. The rebase runs on your host, so if you sign commits
(`commit.gpgsign=true`), co:op signs the merged commits with your key even though the box created
them unsigned. You then run `git push` yourself.

Set a check command for merges with `gate: make check` in
[`.agent/project.yaml`](configuration.md#project-settings-agentprojectyaml).
`COOP_GATE="make check"` overrides it on your machine. Every merge runs the command in the box
against the rebased files. If it fails, co:op stops the merge and your branch stays unchanged. You
still review the changes yourself.

co:op checks for oversized files and changes to files the box hides. This includes secret-looking
filenames and paths matched by a [`.coopignore`](box.md#your-own-paths-coopignore), including one
above the project. It also flags additions, edits, removals and renames of a `.coopignore`, because
these change what future boxes can see.

The scan checks the lines the fork adds for credential patterns from AWS, OpenAI, Anthropic,
GitHub, Slack and other providers, and for random-looking values assigned to secret-named keys, even
if the file's name doesn't look secret. A credential your branch already has doesn't hold up a fork
that edits its file. co:op refuses to merge while your own working tree has uncommitted changes.

co:op asks before merging, then offers to delete the fork after the merge. In a non-interactive
shell, such as CI or a pipe, you must confirm up front, or co:op refuses to run the merge.

| Flag | What it does |
|---|---|
| `--yes`, `-y` | Confirms the merge and the fork's removal up front, which a non-interactive shell needs. In an interactive shell, it skips the prompts. |
| `--force` | Ordinary forks only. Overrides the risky-file policy, but not the check command. Isolated forks cannot bypass the policy. |
| `--all` | Merges all forks in sequence. See [parallel forks](loop.md#parallel-forks). |

A fork loop takes work from the selected task queues. The host assigns each task to one fork
generation, which lasts until you remove or recreate the fork, and places only the assigned task in
the fork. It never mounts the whole queue there. The agent works on that copy of the task, including
its state, artifacts, reviews and final signoff. In the task's original queue, the task stays
`in_progress` while the fork is `reviewing` or `ready`.

After final signoff and signing, co:op records one merge candidate for that fork generation: the
exact HEAD commit, file tree and completed task assignments. Before merging, it checks that
candidate again, rebases it and runs the configured checks, then updates the parent and completes
only the tasks recorded in that candidate. A journal lets co:op resume these steps after an
interruption. A `Coop-Task` trailer helps with consistency checks and search, but it cannot complete
a task on its own.

If a fork can't reach a candidate, for example because its box keeps failing a check your project
passes, you can bring its commit to your branch yourself, such as with `git cherry-pick`. Then run
`coop tasks done <id> --from-fork` in your terminal. It needs exactly one commit on your branch with
that task's `Coop-Task` trailer and a stopped fork with no candidate waiting to merge. If your branch
already had that commit when the fork took the task, such as a loop's commit whose completion was
refused, your branch must also have every commit the fork made since then. co:op adds the tasks the
fork found to your queue, completes the task with a note in its log, and releases the fork's
assignment without deleting the fork. A later merge of the fork skips each commit you brought over
unchanged.

If the final candidate review finds a gap, co:op names the affected tasks and returns them to work
in the same fork before reviewing the complete candidate again. Missing handoff records can be
repaired without another code commit. A repeated finding after an unchanged-history repair, or a
task whose rework cannot be safely authorized, waits for your decision with the finding recorded.

After a stop or crash, the same fork generation can resume its assigned tasks. `rm`, `--fresh` and
the worker API's session discard refuse to proceed while the fork still owns assigned tasks, a
merge candidate, task proposals or cleanup state. `rm` and `--fresh` proceed only with `--force`,
which records how those assignments are handled. `coop tasks watch` shows the project's task states
and every box, including when the status of the process that controls the fork loop is stale or
cannot be determined.
