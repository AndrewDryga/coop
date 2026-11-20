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

If you stop a loop during its final review, run `coop fork <name> --loop` again to finish that
review. The fork takes no new task until the review ends, and `coop fork merge` says the same.

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

An isolated fork keeps its own Git metadata and object files. It starts as a copy of your
repository's branches as they were when you created it, so the agent can read their history. co:op
copies nothing your repository commits after that into it, not when a merge conflicts and not when
one lands. The fork names no Git remote, so a `git fetch` in it can't reach your repository, and
its `origin/*` branches are yours as they were at creation. An isolated fork made by an earlier
co:op release still names your checkout as `origin`. co:op never fetches from it, and
`git -C <workspace> config --remove-section remote.origin` removes it. co:op prevents
the box from mounting your parent checkout or its Git storage, including through another path, a
service or a volume that co:op inspects. The agent can still create commits and branches and run rebase inside the
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
- additions, edits and removals of a `.coopignore`
- tasks added to or changed in your task queue
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

A fork made from a preset (`coop fork perf frontier`) remembers the preset the same way, and
`coop fork perf` and `coop fork perf --loop -d` run it again with its lead's model and account, its
roles and its ladder. If the preset no longer loads, co:op names it and stops. `coop fork perf acp`
runs it in your editor the way `coop acp frontier` does: the lead's model and account, the roles,
and the ladder when the lead reaches a limit. The session stays on that preset; to run something
else in the editor, name a target: `coop fork perf acp claude`.

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

Blocked tasks and proposals already imported into the project's queue stay there. A returned or
blocked task loses the ticks on its subtasks, since the work they stand for goes with the fork, and
its log names the subtasks that were unticked. A returned task's state.md no longer describes the
discarded work as done. Check
`coop fork ls` and `coop fork review` before discarding a fork. [Land it](#land-it) explains the
different effect of `--force` on `coop fork merge`.

co:op keeps each project's fork records in its private state until you delete the forks, so a
repository you delete without `coop fork rm` leaves them behind. `coop fork rm --gone`, run from
anywhere at a terminal, lists the repositories that no longer exist, then asks before it removes
their records and any record folders that hold nothing. `--yes` skips the question but not the
terminal. A repository counts as gone only when it and its forks folder are missing while the folder
that held it still holds other files and is on the same disk as the folder above it. Any other
repository keeps its records: one on a disk that isn't connected, one whose whole parent folder was
deleted, and one co:op can't match to its records, such as one whose forks never took a task: their
records name no repository. A repository you moved loses its fork records.

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
2. `git config --global core.editor`, your normal Git editor (co:op reads only the global value,
   including files it `[include]`s)
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
that edits its file. co:op refuses to merge while your own working tree has uncommitted changes to
tracked files. An untracked file stops the merge only when the fork would land on it, and co:op
names it.

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

Forks that work in parallel often change the same files, so after you merge one, the next one's
reviewed commits can conflict with your branch. Then nothing merges, and the refusal names the
files and the next step:

```sh
coop fork <name> --loop     # its agent rebases the work onto your branch and resolves the conflict
coop fork merge <name>      # once the rebased work passed its review
```

Until then `coop fork ls` shows the fork as "conflicts with your branch". The loop runs one pass
whose agent rebases the fork's branch onto the commit your branch is at when the pass starts, keeps
every commit message with its `Coop-Task` trailer, resolves the conflict, and runs your merge gate
and `git diff --check` on the result. co:op then checks that every task still has exactly the
commits it had. The rebased work goes through the same full candidate review as new work, because no
earlier review saw it, and the reviewer gets the checks the pass ran. Only that reviewed commit can
merge. If the loop stops before the review, run `coop fork <name> --loop` again: a finished rebase
goes straight to review, and an unfinished one, even one stopped in the middle, is redone, so never
reset the fork by hand. Pressing Ctrl-C once during the rebase pass, the check-only pass below or a
review lets that pass finish and keeps what it did, then stops before the next pass; pressing it
again stops at once. You can also rebase the fork yourself, for example when co:op names the
`git rebase` that straightens a merge commit. Then no pass reported checks on your result, so the
loop first runs one check-only pass: its agent runs your merge gate and `git diff --check` on the
rebased commits, changes nothing, and the reviewer gets what it reports. If that pass changes the
fork, co:op stops before the review. A merge gate that writes to tracked files does that every time,
so have it write its output only to ignored paths. If the rebase leaves none of the fork's commits because your branch already
has the work, there is nothing to review or merge, and the loop says so: discard the fork's work
with `coop fork rm <name> --force`, which returns its tasks to the queue, and decide there what they
still need. If it left none because the rebase dropped them, co:op says which changes are missing:
go back to the reviewed commit it names and run the loop again. You can also change your branch
until the reviewed commits apply and merge again, or discard the fork's work.

co:op reviews and merges only a straight line of commits. A merge lands a fork's commits one by one
on top of your branch, so a merge commit among them could bring back a change that the merge's own
diff, the one the review read, does not show. A fork's loop therefore refuses to start a review of
commits that include a merge commit and names the `git rebase` that straightens them; `coop fork
merge` and `coop fork ls` say the same, and while a conflict waits for its rebase the loop says it
before any agent runs. A reviewed round that has one anyway is not merged: for an ordinary fork,
`coop fork merge` names the `git rebase` onto your branch that straightens it. No agent runs for it;
once you have run that rebase, the fork's loop checks and reviews the result like any rebase you
made by hand.

A merge also checks that it lands exactly what was reviewed. When your branch has moved, the
rebased commits must make the same line changes as the reviewed ones. When they don't, for example
because your branch already has part of the change, the fork goes to the same rebase and review as
after a conflict, and `coop fork ls` shows it as "changed after review". A rebase skips a commit
whose change your branch's history already has, and git ignores whitespace when it compares
changes, so a commit still counts when your file differs from the fork's only in whitespace. When that commit is one a task's `Coop-Task` trailer binds,
the task would be left without a commit of its own, and no rebase or review can give it one back. So
the merge stops at once and says so, even when the rest of the change would land exactly as
reviewed, and the loop then runs no agent for it. When a conflict came
first, the loop's rebase pass is what finds it: that one run ends with the same answer, and the loop
runs no agent after it. Discard the fork's work with `coop fork rm <name> --force`, which returns its
tasks to the queue, or remove your branch's commit with that change from its history and merge
again. Reverting it is not enough: a rebase skips a commit whose change appears anywhere in your
branch's history since the fork started, even if a later commit undid it. Once a merge gets past
this check, the refusal is gone, even if your merge gate then fails.

A fork whose repository is shallow is refused until its `.git/shallow` file is removed, since git
would then misread its history; until then `coop fork ls` shows a reviewed one as "changed after
review". An `--isolated` fork made from a shallow clone keeps that clone's shallow file, which is
fine; its merge and review refuse only a shallow file that cuts into the fork's own commits.

This path is for ordinary forks. An `--isolated` fork holds your repository's branches only as they
were when it was created, and co:op copies none of your branch's later commits into it, so it cannot
rebase the fork onto your branch for its agent. When its reviewed commits conflict with your branch, `coop fork ls`
shows it as "conflicts with your branch" and the merge names two ways out: change your branch until
the fork's commits apply and merge again, or discard its work with `coop fork rm <name> --force`
and start a new isolated fork from your current branch. Its loop has nothing to do, since no agent
can rebase it: `coop fork <name> --loop` refuses at once with the same ways out, and names the
command that starts a new isolated fork if you choose to discard this one. The merge gives the same two ways out when
the reviewed commits apply but would not land exactly the reviewed change, for example because your
branch already has part of it or has the change of a task's commit; `coop fork ls` then shows it as
"changed after review" with those ways, and its loop refuses the same way. Both show only while your
branch is where that merge found it: once you change the branch, `coop fork ls` shows the fork as
ready again, and the next merge checks it anew. Don't cherry-pick its commits into your checkout by hand, since that skips the checks co:op runs on an isolated fork's commits, and don't
redo untrusted work in an ordinary fork, which gives the agent your checkout's Git storage.

A fork whose branch moved after its final review shows as "changed after review" in `coop fork ls`,
and `coop fork merge` refuses it. New commits on top of the reviewed one need another review, which
the fork's loop gives them. Any other branch that no longer builds on the reviewed commit, such as
one you rewrote by hand with no conflicting merge waiting, can neither merge nor be reviewed again:
reset the fork's branch to the reviewed commit that the refusal names, or discard the fork's work
with `coop fork rm <name> --force`. If a rebase of yours is still in progress in the fork, the
command it names aborts that rebase first, which drops any conflict resolution you have not
committed.

A loop that stops prints a `Continue:` line that repeats the exact run you started, its agent and
options included. `coop fork <name> --loop` resumes it as well, with the fork's saved agent.

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
