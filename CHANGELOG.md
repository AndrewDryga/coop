# Changelog

## Unreleased

<!-- Add entries here as you ship; this heading is renamed to the version on the next release. -->

- **A session turn that runs out of time says which step never finished.** A turn that hit its
  deadline failed with "turn deadline exceeded" whether the agent never answered or never read its
  prompt. The error detail now adds "while waiting for the agent to answer session/prompt" (or
  `initialize`, `session/new`, `session/load`), or "while sending session/prompt to the agent".

- **Approve each filtered project's network access once after upgrading.** Coop named each
  project's network records with a secret key kept beside them, so losing that key would have
  lost every approval for good. The records are now named by the project's folder alone. Coop does
  not move old approvals: the first filtered run in each approved project stops, says Coop renamed
  its network records, and asks you to run `coop approve`, which shows what changed since you last
  approved. `coop net forget` takes an old approval back too. A project whose access you took back
  before upgrading stays that way until you approve it again. Worker sessions created before the
  upgrade can no longer find their network rules, so start them again. The record `coop net setup`
  writes changed too; this release's new images already need a new setup.

- **`COOP_REPO` typed in another letter case names the same project.** On a Mac's disk, which
  ignores letter case, `COOP_REPO=~/projects/APP` and `~/Projects/app` are one folder, but Coop
  kept the typed spelling, so task claims, fork records and network approvals split between the
  two spellings and two loops could claim the same task. Coop now uses the folder's own spelling.
  If you set `COOP_REPO` in a case other than the folder's, task claims and network approvals
  made under that spelling are not found once: approve the project's network rules again with
  `coop approve`.

- **One unreadable network run record no longer stops cleanup of the others.** `coop net recover`
  and the pass before each filtered launch cleaned up nothing when one recorded run could not be
  read. They now clean up every run they can read and never remove anything because of a record
  they could not read, and `coop net recover` names each record it could not read.

- **A box can no longer fake another project's fork records.** Coop kept each project's fork
  records (which forks exist, their workers, locks and logs, reviews, and lands and discards in
  progress) in `<project>-forks/.coop`, next to the project. A box whose folder held another
  project's forks folder (a project nested in yours, or a box started in a folder of several
  projects) could write them, and one fake record made `coop fork stop` stop another running Coop
  session. The records now live in Coop's private folder, `~/.local/state/coop/forks/`, which no
  box can mount. The first time this version opens a project, it copies the project's records
  there, following no link, and never reads the old place for that project again. Forks now need
  each fork's folder and your home folder on one disk: Coop refuses a fork on another disk and
  names both folders. An older Coop that opens the project afterwards does not see its forks; see
  MIGRATING.md.

- **A box or service on a Mac can no longer read a hidden file under another letter case.** Coop
  hides a secret file such as `.env` by covering it with an empty file. A Mac's disk ignores letter
  case, so a box or a project service could `cat .ENV` and read the real file, or edit a
  `.coopignore` by writing `.COOPIGNORE`. Coop now covers every spelling of each hidden file's name
  and of each `.coopignore`, in the box and in each service that binds the repository. A name too
  long to cover that way (more than 11 letters, such as `.env.production`) stops the launch or
  `coop up`: move the file into a hidden folder (a folder is hidden under every spelling), give it
  a shorter name, or set `COOP_ALLOW_CASE_EXPOSED_SECRETS=1` to start anyway. Linux disks, which
  keep letter case, are not affected.

- **Fork merges and reviews check every spelling of a file.** A Mac's disk ignores letter case, so
  a commit that adds `.GITHOOKS/pre-commit`, `.AGENT/compose.yml` or `.GITATTRIBUTES` puts a file
  where Git, your editor or Docker runs it as the lower-case one. `coop fork merge`, session reviews
  and the loop's check for changed gate files looked only at the exact spelling, so such a change
  went through without the usual warning. They now treat every spelling of a host-run file, a gate
  file, or a name a `.coopignore` hides as that file, on every disk.

- **A box can no longer point its task tools at another project's tasks mid-call.** The task tools a
  box calls run on your machine. Each one checked that the queue's folders were real, then found
  them by name again, so a box that swapped `.agent` or a state folder for a link at the right
  moment could have a call list or change another project's tasks. Each call now holds the folders
  it checked and does all its reads and writes through them. A folder a box swaps for a FIFO no
  longer makes a call hang.

- **Task commands no longer follow links a box planted in the task queue.** `coop tasks block`
  wrote its `decision.md` template through a link a box could leave there, which could create a file
  anywhere you can write. Task moves, cleanups and deletions found state folders by name, so a box
  that swapped one for a link could send them to another folder. They now refuse such links and work
  through the queue's own folders.

- **A `.COOPIGNORE` on a Mac is read-only in boxes too.** On a case-insensitive Mac disk, Coop reads
  a folder's `.COOPIGNORE`, or the name in any other mix of cases, as its `.coopignore`, but it
  mounted only the exact lower-case name read-only. A box could edit such a file and change what
  the next launch hides. Every file Coop reads as a folder's rules is now read-only in boxes and
  service containers, and `coop fork merge` and session reviews flag a change to one as they do for
  `.coopignore`.

- **A loop attempt that fails is checked for forged task commits too.** `coop loop` refuses a
  completion whose commits carry another task's `Coop-Task` trailer or rewrite another task's
  commit, but it checked only the attempt that completed its task. A box could make such a commit
  in an attempt that then failed, and the retry started above it and completed cleanly. Every
  attempt's commits are now checked when it ends; such a commit stops the run with "Task completion
  rejected" and puts the task back in progress.

- **An agent's "Internal error" now says what failed.** When an agent's adapter hit an exception it
  did not expect, it answered with the bare words "Internal error" and put the exception's own
  message beside them. Coop kept the two words and dropped the message, so a turn that failed that
  way was recorded as `ACP request was rejected: Internal error` and nothing else. A worker failed
  ten turns that way in 40 minutes, and nothing on record said why. The turn's error now carries
  the adapter's message after those words, as one bounded line.

- **`coop doctor` no longer uses your git config for its test project.** `coop doctor` builds a
  throwaway project, commits it and clones it with git. With commit signing turned on in your git
  config, that commit was signed with your key, or failed where gpg was missing so doctor reported
  a problem that was not there. Your global git hooks ran in it too, and a failing one failed the
  check. Its git commands now run without your git config or git environment variables, as
  Coop's other throwaway repositories already do.

- **A preset folder whose name holds a control character is no longer a preset.** An agent in a
  box can create folders under `.agent/presets`, and `coop presets` printed a preset's folder name
  as it was, so an escape sequence in it reached your terminal. Such a name is now refused like
  any other unusable preset name, listed as a preset that could not load, with the name shown
  escaped, and every name `coop presets` lists is escaped.

- **Coop shows invisible characters in names an agent chose.** Where Coop escapes a name an agent
  in a box could have chosen, such as a file, folder or service name, it shows the control
  characters in it as codes, such as `\u001B`. Invisible characters, such as a zero-width space, a
  byte-order mark, a soft hyphen or a variation selector after a plain letter, and the Unicode line
  and paragraph separators, were printed as they were, so a folder named `w\u200Beb` looked exactly
  like `web`. Now Coop shows them as codes too. An emoji joined from several, such as a family,
  shows its parts with `\u200D` between them.

- **Task titles an agent wrote can no longer hide characters or turn a row around.** An agent in a
  box can write a task's title and its decision. Coop dropped control characters such as an escape
  from them before showing them, but kept the others that act on a terminal or show as nothing:
  the controls above the ASCII range, a right-to-left override that reverses the text after it,
  the Unicode line and paragraph separators, and invisible characters such as a zero-width space.
  Now it drops those too. When Coop lists several commits that claim one task, their authors show
  the same characters as `?`, and a filtered run refuses a remote MCP server whose name holds one,
  as it already refused one holding an escape.

- **Coop no longer writes fork records through a link an agent put in their place.** Coop keeps
  each fork's records in `<project>-forks/.coop`. An agent that could write the forks folder could
  replace `.coop` with a link while Coop was writing, and the record went where the link pointed.
  Every record there is now written through the real folder, and a link in its place is refused.

- **`coop fork rm` and `--fresh` keep work a fork's services wrote while stopping.** Coop checked a
  fork for uncommitted work, then stopped its services and deleted it. A service that could write
  to the fork kept writing until it stopped, and a service that failed to stop was ignored, so that
  work was deleted. Coop now checks the fork again once its services have stopped, and keeps it
  when they did not stop cleanly or when new work appeared; `--force` still deletes it.

- **A stuck git command no longer hangs Coop forever.** Coop runs git on repositories an agent can
  write, and a FIFO put in a repository's `.git` makes git wait forever. Coop now stops any git
  command that runs longer than 15 minutes and says which repository it stalled in. Commands you
  interact with, such as a pager or a signing prompt, are not stopped.

- **Leftover containers of finished remote sessions are cleaned up.** A session run's credential
  helper (`coop-native-*`) could outlive the run, and since the run's workspace was gone, no cleanup
  ever reached it. Coop's sweep now removes a session run's leftover containers from any project,
  when the Coop that started the run on this computer is provably gone.

- **A FIFO put in the forks folder no longer freezes `coop fork` commands.** An agent can write the
  folder that holds a project's forks, and a FIFO or link put where `.coop` or the forks folder
  belongs made `coop fork ls`, `stop` and `merge` wait forever, or read another folder. Coop now
  opens those folders without waiting and refuses a link, so the command stops with an error.

- **A fork worker started in a container is no longer judged dead by the host.** On Linux, Coop
  identified a process by its pid and start time, and a pid means something only in the PID
  namespace it was read in. So a Coop on the host reading a record that a Coop in a container wrote
  (they share the project's fork folder), or the reverse, saw a live worker as gone and could stop
  treating it as running. Coop now records the namespace with each process, and a record from
  another one is refused with "started on another container or host", as a record from another
  operating system already was. Records written by earlier versions are read as before.

- **A network run started on another computer is left to that computer.** A filtered run's record
  named its supervising Coop process by pid only, so a Coop on another computer or in a container
  that shares the same state folder read that pid as gone and could seal the run as lost while it
  was still finishing. Each run now records which host started it, and cleanup skips a run from
  another host and says so.

- **A fork loop whose agent is not signed in stops before it makes the fork.** `coop fork <name>
  --loop` checked its own agent's sign-in only after cloning the fork, and with `-d` the error
  showed only in the fork's log. It now stops with the sign-in message before anything is made,
  and with `-d` prints it in your terminal.

- **A fork that changes a `.coopignore` needs `--force` to merge.** A `.coopignore` decides what
  every later box is shown, and a fork could edit one to drop a rule and merge without a word,
  after which the next boxes saw the files that rule hid. `coop fork merge` now names any
  `.coopignore` a fork adds, edits, removes or renames and waits for `--force`, and the session
  review lists the same.

- **`coop fork merge` no longer merges a fork it could not check.** Before merging, Coop checks
  the fork's changes for secrets, hooks and other risky files, and a git error while reading the
  fork read as "nothing found", so the merge went ahead. A fork's agent can cause that error, for
  example with a history unrelated to the project's. The merge now stops and says the fork could
  not be checked, unless you pass `--force`, and `coop fork review` shows the same finding.

- **A subproject folder with an odd name no longer breaks project.yaml.** `coop init` wrote each
  subproject's folder name into `.agent/project.yaml` as it was, so a name holding a colon and a
  space, a `#`, a quote or a control character made the file read a different value, or not load
  at all, for every later command. Such a name is now written in quotes and reads back exactly;
  one that cannot, such as a name that is not UTF-8, is refused with the name shown.

- **`coop tasks block` no longer asks for a decision that is already written.** Blocking a task
  whose decision.md an agent had already filled in still said to write the question, options and
  recommendation, and blocking a task that was already blocked said "Blocked task" as if it had
  moved. The first now only names the file, and the second says the task is already blocked and
  changes nothing.

- **Lock files of deleted projects are cleaned up.** Every box launch keeps a small lock file for
  its project's folder in `~/.local/state/coop/executions/v1/service-locks`, and none was ever
  removed, so every temporary project, such as the one each `coop doctor` run makes, left one
  behind: one Mac held 62,230. Each lock now records its project, and Coop's daily cleanup removes
  up to 10,000 a day whose project is gone, never one in use.

- **Coop no longer makes your home folder a project.** Outside a Git repository Coop used the
  current folder as the project, so `coop init` or `coop shell` run from your home folder set it
  up or mounted it read-write into a box, and so did a folder reached through a link that leads
  there. Such a folder is now refused, as a repository at your home folder already was. `coop login`
  and `coop doctor` need no project, so they now run from any folder, including a home folder that
  is a Git repository: sign-in mounts no folder of yours, and doctor checks the shared image and
  says it looked for no project's abandoned boxes.

- **A blocked task's decision now says why it is blocked.** `coop tasks block` takes `--why`,
  what happened and why the task cannot go on, and the agent's block tool asks for the same. It is
  written under the task it blocks, and the question becomes the decision's heading, so
  `coop tasks decisions` lists the question itself instead of the task's title. A decision request
  now needs a question on one line, why, at least one option and a recommendation. The empty
  decision.md that `coop tasks block` writes has a place for why, too.

- **A task a fork proposes can no longer be written outside the queue.** When `coop fork merge`
  imported a task a fork's agent proposed, it wrote the task's files and moved its folder into the
  queue by path. A box working in the same project can write the queue, and a folder it swapped
  for a link made the import write where the link pointed. The import now writes and moves
  through the queue's folders themselves and fails on a link, as `coop tasks add` already did.

- **An error Coop prints no longer passes terminal codes through.** When a command failed with an
  error Coop does not draw as its own block, the error was printed as it was, and it can quote
  text a box wrote: a project.yaml key or value, a file name, git's output. An escape sequence in
  that text reached your terminal, and a line break in a key or file name started a line of its
  own. Such characters now show as their codes, while the line breaks an error puts there itself
  stay.

- **`coop loop` exits 1 when the work it leaves is not finished.** A loop that stopped because
  another agent holds the rest of the queue exited 0, the code for a queue that passed review,
  though that work was not done. When the final review hit its round limit and Coop could not
  move the reopened tasks to blocked, the loop exited 3, the code for a run that stopped for your
  decision, though nothing was blocked. Both now exit 1, as `coop help loop` says for work that
  remains.

- **`coop loop` exits 1, not 190 or 191, when an agent keeps leaving background work running.**
  After an agent ended three times while work it had started in the background was still running,
  `coop loop` stopped and exited 190 or 191, a status from inside the box that `coop help loop`
  does not list. It now exits 1, like its other stops that leave a task in progress, and
  `coop eval` no longer reports such a loop trial as a loop that could not start.

- **`coop loop` stops when it cannot read which files a task changed.** The loop lists the files
  each completed task changed to see whether it edited the project's checks: the Makefile, CI,
  hooks, Coop's own config or a `gate_sources:` path. When Git could not list them, for example
  because the agent deleted an object or damaged a ref in the repository, Coop took that as "no
  checks changed", skipped the protected audit and went on to the next task. Now it stops with
  "Could not list the files this task changed" and the Git error. Continuing with the command it
  prints reviews that task before any other work, and the final review stops instead of running
  without the changes.

- **Coop's home-folder checks also see your home folder through a second mount.** macOS reaches
  your home folder as `/System/Volumes/Data/Users/<you>` as well as `/Users/<you>`, also when it
  is on another disk, and a Linux bind mount, such as `/data/home` mounted at `/home`, reaches it
  under a second folder too. Coop refuses a `COOP_REPO` link to your home folder or above, and a
  Git search that climbs there, but did not count the folders above that second path, such as
  `/System/Volumes/Data`, `/System` or `/data`; now it does.

- **`coop init` now escapes control characters in the names a box chose.** An agent in a box can
  name the task queue folders `coop init` registers and your project's own Dockerfile and Compose
  file, and can set the `box.dockerfile`, `box.compose` and `core.hooksPath` values it quotes. Any
  of them can hold terminal control characters that repaint the output with a fake line, hide text
  or, on some terminals, write to the clipboard. `coop init` and `coop init --services` now show
  those characters escaped, as `\u001B` for example, and so does the warning about network access
  Coop could not check, which can quote `.agent/project.yaml` or the Compose file.

- **A fork's submodule filters no longer run on the host during cleanup.** After landing a fork,
  Git could read a populated submodule's box-written configuration while checking for leftover
  work. Coop now checks each child through its own trusted Git view and prevents recursive
  submodule commands across host workspace operations. Dirty or untracked child work still
  keeps the workspace, including during `fork rm` and `fork --fresh`; changed submodule commits
  still appear in reviews. Release tag verification also ignores checkout transport settings and
  takes its remote URL from the release workflow.

- **A `.agent/loop.yaml` or preset that does not load now exits 1 instead of 2.** `coop loop`,
  `coop acp`, `coop fork`, `coop <preset>`, `coop presets <name>` and `coop help <name>` exited 2,
  the code for a mistyped command, when the loop's settings file or a preset's `preset.yaml` did
  not load. They now exit 1, the code for a command that could not do its job, as a `coop.conf` or
  `.agent/project.yaml` that does not parse already does. Naming a preset that does not exist still
  exits 2. `coop eval` now refuses a loop recipe that does not parse before it starts any trial,
  instead of scoring each trial's failed start. `coop help loop` lists the exit codes as they are.

- **`coop eval` no longer grades a loop trial whose loop never started.** A loop that stops before
  its first box, for example because the image is missing or the account is not signed in, exits 1
  just like a loop that ran and left work. `coop eval` could grade such a trial against the
  configuration, and otherwise said the agent had changed nothing. The loop now records when its
  first box starts, somewhere the agent under test cannot reach, and a trial without that record is
  an error that says the loop exited without launching a box, followed by the loop's own message. A
  loop that ran and left work is still graded.

- **A large companion repository is ready in half a minute, not five.** Coop copied each companion
  repository a session reads into a new Git repository the way a clone over the network does: Git
  searched its large files for deltas again and checked every object once more. A 6 GB repository
  took five of the six minutes a Work session waited before its agent started. Coop now writes the
  pinned commit's history straight from the worker's copy, as it is stored: the same repository
  takes about half a minute. Only that commit's history is copied; other branches and objects no
  commit reaches stay behind. Submodules of a session's repositories are copied the same way.

- **Coop records what each prompt it sends carried.** A controller showing its own prompt showed
  only part of what the model got: Coop's output contract, its correction messages and the
  output-folder note were not recorded anywhere. Each send now leaves a `prompt.sent` session event
  with Coop's text as sent, the caller's prompt and output schema by digest, and the digest of the
  whole text, so a controller can rebuild the text and check it. Workers forward the event to their
  controller; a part that looks like it holds a secret goes as its digest and size only.

- **A session with many companion repositories starts sooner.** Coop prepared a session's
  companion repositories one after another, so a Work session with sixteen of them waited about six
  minutes before its agent started. Now it prepares up to four at once. A failure still stops the
  others and names the repository that failed.

- **A fork loop whose `--peer` agent is not signed in no longer clones the fork first.**
  `coop fork <name> --loop --peer <agent>` checked the peer's login only after it had cloned the
  fork, so a missing login cost a full clone and left the new fork behind. With `--fresh` it asked
  to recreate the fork and, once you agreed, recreated it before refusing. Coop now checks each
  peer's login first, so a refused peer leaves your forks as they were.

- **Coop no longer follows a link in `COOP_REPO` to your home folder.** When `COOP_REPO` is a
  folder inside a project, such as a worktree kept in it, an agent in a box working in that
  project could replace the `COOP_REPO` folder with a link to your home folder. The next box then
  got your whole home folder, apart from the files Coop hides. Coop now refuses a `COOP_REPO` that
  a link takes to your home folder or a folder above it, and names the folder the link reached.
  Setting `COOP_REPO` to your home folder itself still works. A box that can write the folder
  holding `COOP_REPO` can still replace it with a link to another folder, such as `~/.config`,
  which the next box gets read-write, or swap it just after Coop checks it. Keep `COOP_REPO` out
  of folders a box can write.

- **`coop init` no longer freezes on a named pipe a box leaves in the project.** `coop init` reads
  `.tool-versions`, Coop's `prepare-commit-msg` hook and every project folder while it looks for
  subprojects, and `coop help` reads the Compose file for its service list. An agent in a box could
  leave a named pipe at any of them, and the command waited forever. Now none of them waits:
  `coop init` refuses a `.tool-versions` that is not a regular file of at most 1 MiB and names it,
  `coop help` leaves the service list out, and the subproject search skips the folder. A
  `.tool-versions` or Compose file that links to another file in the project still works. The
  files `coop init` edits in place, `.agent/project.yaml`, the Compose file and `.gitignore`, are
  refused over 1 MiB too, so a box cannot make it read a huge file into memory.

  `.tool-versions` and Coop's `prepare-commit-msg` hook, and `coop help` reads the Compose file for
  its service list. An agent in a box could leave a named pipe at any of them, and the command
  waited forever. Now none of them waits: `coop init` refuses a `.tool-versions` that is not a
  regular file of at most 1 MiB and names it, and `coop help` leaves the service list out. A
  `.tool-versions` or Compose file that links to another file in the project still works. `coop
  init`'s search for subprojects, and for the project's own Dockerfile and Compose file, also no
  longer follows a link a box swaps in for a folder: the subproject search stops and names that
  folder, a subproject's `.agent` must be a real folder, and the Docker search skips it.

- **Coop's own search for a project's `.git` no longer ends in your home folder.** Before running
  Git in a project or fork, Coop looks for the nearest `.git`, starting in that folder and moving
  up. If an agent in a box deleted the project's own `.git` while your home folder, or a folder
  above it, is a Git repository, the search ended there, and a loop's commits, signing and reviews,
  `coop fork merge` and session clones could read and write that repository. Coop now stops with an
  error that names it. Before updating a branch or `HEAD`, where Git searches for the `.git`
  itself, Coop also stops when the folder's `.git` is one Git cannot use, such as one the agent
  emptied, because Git would pass over it and climb on. Choosing the project already refused such
  a repository, and `COOP_REPO` can still name your home folder.

- **Coop no longer treats a process recorded on another operating system as stopped.** A Linux
  container that shares a project, or Coop's state folder, with macOS leaves records that name its
  own processes: a fork's background loop, a task claim, a filtered run. Coop checked such a pid on
  its own system, found it missing or taken by another process, and treated that process as
  stopped: `coop fork stop` removed the fork's boxes and its record while the loop could still be
  running, `coop loop --preflight` released the claim, and network cleanup removed the run's
  containers and volumes. The same happened the other way round. Coop now keeps these records:
  `coop fork stop` says which system started the loop and to stop it there, or which file to
  remove once that system is gone. `coop fork ls` and `coop tasks watch` show the fork as
  "background loop started on Linux", not as needing cleanup: in their `--json`, `cleanup_pending`
  is false and `detached_system` names the system, present only then, and `coop fork ls --json`
  gives the state `elsewhere`. `coop tasks ls` shows a claim's pid as "PID 4242 on Linux".

- **Breaking: read-only and bare runs are removed.** `--readonly` and `--bare` no longer run
  anything. `coop <agent>`, `coop <preset>`, `coop run`, `coop shell`, `coop acp` and
  `coop fork <name> acp` refuse them as unknown options and say what to do instead: every box is
  writable, a read-only second opinion is `--peer <target>`, isolation is a fork
  (`coop fork <name> <target>`), and `--bare` has no replacement. Claude Code's own `--bare` still
  works after `--`, in a normal writable box. A session job asking for `mode: "readonly"` or
  `"bare"` is refused with `invalid_request`, which says what to send instead: `mode: "normal"`,
  with `repository_read_only: true` for a job that must not change its repository. Those jobs work
  as before. A session created in a removed mode stays readable but can't run again; close and
  discard it.

- **A `.coopignore` Coop cannot read now stops the box instead of hiding nothing.** When a
  `.coopignore` in your project was a link, a folder, a named pipe, a file over 1 MiB or one Coop
  had no permission to read, Coop dropped its rules, so the files it named were visible in the box.
  Now starting or building a box refuses and names that `.coopignore` and why, and so do
  `coop up`, `coop check-secrets`, `coop init` and a preset stored under it; `coop loop` stops at
  the first launch that meets one instead of retrying it. `coop fork merge` asks for `--force` and
  session reviews report it. Both still name every file under it that your other `.coopignore`
  rules hide, or that its name or a folder above it marks as a secret, such as `.env` or anything
  in `secrets/` or `.aws/`.

- **`coop init` no longer freezes when a box swaps the folder of a file it edits.**
  `coop init --services` and the registration of new task queues in `.agent/project.yaml` edit a
  file in place. An agent in a box could swap that file's folder for a named pipe just as
  `coop init` opened it, and `coop init` waited forever. Now it stops at once and names the folder.

- **A missing sign-in, or a preset a loop cannot run, now exits 1 instead of 2.** `coop acp --peer`
  and a fork loop with `--peer` exited 2 when their own agent had no account to run, though
  `coop loop` exits 1 for the same problem. Any run exited 2 when a `--peer` agent was not signed
  in, and `coop loop` and fork loops exited 2 when an agent in `.agent/loop.yaml` could not host a
  native role of the loop's preset. Exit 2 is for a mistyped command, and these were typed
  correctly, so they now exit 1 with the same messages. A `--peer` value Coop cannot take, such as
  one that names an account, still exits 2. Coop now names it before checking any account, wherever
  it sits among the peers, so a fork loop no longer creates the fork first.

- **A box can no longer make Coop change files outside a fork's state folder through its lock, log or
  loop record.** Coop keeps each
  fork's lock, log and loop record in `<project>-forks/.coop`. A box started in a folder that
  contains it, such as a parent folder of the project, could put a link at a fork's lock, and Coop
  created the file the link pointed to or made it readable only by you. The box could also swap
  `.coop`, or the forks folder, for a link just after Coop checked it, and Coop then emptied,
  replaced, deleted or changed the permissions of files in the folder the link pointed to. Coop now
  opens the forks folder and `.coop` without following a link, and creates, renames and removes
  these files only inside the folder it opened. It refuses a link, a named pipe or anything else
  that is not an ordinary file at a lock or a log, and a background start and `coop fork stop` say
  what they found and how to remove it.

- **A session whose box image is missing says so even when it fails fast.** When the model process
  of a session exited before Coop sent it anything, the turn failed with "ACP frame could not be
  written" instead of the advice to run `coop build`. Coop now reports such a failure the same way
  as one it notices while reading, with the same safe diagnostic.

- **A filtered box can no longer send names out through Docker's own name server.** When a
  filtered run has Compose services, Docker answers names inside the box's network at 127.0.0.11.
  Coop sent the box's lookups on the usual port to its own guard, but Docker also listens there on
  other ports and passed a name it did not know on to your Docker engine's resolver. On OrbStack,
  and on Linux hosts whose resolver runs on the machine itself, it did so from outside the box's
  network: the box never saw an answer, but the name left your computer, so an agent could carry
  data out in the names it looked up. A filtered box can no longer reach 127.0.0.11, so its only
  name server is Coop's guard. With Docker Engine 26 or later, the Compose services were not
  affected: on their network Docker answers only their own names.

- **A box can no longer freeze fork commands through a fork's state files.** Coop keeps each
  fork's loop record and log in `<project>-forks/.coop`. A box started in a folder that contains
  it, such as a parent folder of the project, could put a named pipe there, which froze
  `coop fork ls`, `coop fork stop`, `coop fork logs`, `coop tasks watch` and `coop prompt`, or a
  link to an endless device, which they read until memory ran out. Now Coop opens these files
  without following a link or waiting on a pipe, reads only ordinary files, refuses a loop record
  far larger than any it writes, and prints a very long log line in pieces. Anything else is
  refused at once, and Coop says what it found: `coop fork ls` warns about that fork,
  `coop fork stop` says how to clear it, `coop fork logs` says how to remove it, and the other
  forks keep working.

- **A `.coopignore` now covers repositories inside its folder.** An agent in a box could make any
  folder of your project a repository of its own by creating a `.git` in it. A Coop command run
  from inside that folder then took it for the project and read only the `.coopignore` files in
  it, so a file your project's `.coopignore` hides there, `sub/secret.yml` say, was visible in the
  next box. Coop now also applies the `.coopignore` of every folder above the project, up to but
  not including your home folder (for a project outside your home folder, up to the folder just
  below `/`). Starting Coop inside a submodule or a worktree kept in your project now also applies
  your project's rules. A `.coopignore` in a folder above your projects, such as
  `~/Projects/.coopignore`, now applies to each of them: boxes no longer see what it hides, `coop
  fork merge` refuses a fork that changes such a file unless you pass `--force`, and an isolated
  fork cannot publish one at all. One of these Coop cannot read stops the box, as one inside the
  project does. `~/.coopignore` does not apply to the projects below it, and a `.coopignore` in a
  folder another user owns, such as `/tmp`, is ignored.

- **The task tools in a box refuse a task queue reached through a link.** In `coop loop` and
  `coop fork`, the agent manages its tasks through task tools that Coop runs outside the box. Coop
  found each task queue by its path, so an agent could replace a folder on that path with a link to
  another project: `.agent`, a subproject folder, a fork's task folder, or a queue's `00_todo` or
  `xx_backlog` folder. The tools then listed and read that project's tasks, changed them, and added
  tasks or proposals to it. The tools now refuse a queue, or a state folder, reached through a link.
  This also refuses a setup that used to work: if a subproject folder or a task queue folder is a
  link, `coop loop` now names the queue and stops at startup. Replace the link with the real folder,
  or point `project.yaml` (or `--tasks`) at the real folder. `coop tasks add` and `coop backlog add`
  refuse a state folder that is a link. They, and a fork loop adding a task its agent discovered,
  also refuse a queue `README.md` that is a symbolic link: replace the link with a copy of the file,
  or delete it to use the built-in template.

- **A filtered run killed as it finished no longer shows `final record missing` forever.** If the
  `coop` running a filtered box was killed after it removed the run's containers, volumes and files
  but before it wrote the run's final record, nothing wrote that record later: Coop finished only
  runs still waiting for cleanup. `coop net runs` kept the run marked `final record missing`, and
  Coop never removed its record. The next filtered launch, or loop, fork or build start, now writes
  the record as it does for other interrupted runs. `coop net recover`, or `coop net inspect` of that
  run, writes it at once, even when Docker is not running.

- **Coop no longer removes boxes that another computer is running.** Coop removes a box once the
  Coop process that started it is gone. When two computers, or a computer and a dev container, used
  one Docker with the project at the same path, neither could see the other's Coop processes and
  took them for gone, so the next `coop loop`, `coop fork` or `coop build` on one removed the boxes
  the other was running. Each box, and each loop's or editor session's services, now records which
  computer or container started it, and Coop removes only its own. Coop keeps a random id for the
  computer in `~/.local/state/coop/host-id.json`. Update Coop on every computer and dev container
  that uses the same Docker: a box that an older Coop starts does not record where it runs, so
  another computer's Coop can still remove it. A Coop running inside a container leaves such boxes
  alone, since it cannot tell whose they are. When a container that runs Coop restarts, what its
  crashed runs left before the restart now counts as another container's, so Coop no longer removes
  it on its own. `coop doctor` now lists the boxes and services that another computer or container
  started for the project, with the commands to remove them once that Coop is gone, and says how to
  fix the id file when Coop cannot read it.

- **A paused final review finds each task's commit by its `Coop-Task` line.** A loop stopped before
  its final review remembered the commit IDs of the tasks still waiting for it, so any rewrite Coop
  had not recorded itself, such as signing the commits yourself or amending one, stopped the next
  loop with a history-changed error. Now the review looks for each task's commit after the commit
  its run started from, and stops, naming the task, when that commit is missing or appears twice.
  A task another session committed before the loop started and completes during it is still
  reviewed, at its one commit in the branch. A task whose commits change after its review passed
  is reviewed again. Review prompts now tell agents to find a task's commits by that line rather
  than by a commit ID from its notes, which signing makes stale. `coop loop --review-task <id>` on
  a task that already passed final review now reviews it again instead of refusing.

- **Coop no longer lets a box's Git settings choose the project folder.** Coop asked Git where your
  repository starts, and Git follows a setting in `.git/config` that an agent in a box can change. A
  box could point it at another folder, such as your home folder, and the next Coop command used that
  folder: `coop init` set it up, a launch mounted it into a box, and `coop net` approved or forgot
  rules for it. Coop now takes the nearest folder that holds `.git`, never one another user owns, and
  no longer climbs to a repository at your home folder or above it. If your home folder is a Git
  repository, run Coop in a project folder with its own repository (`git init` there), or set
  `COOP_REPO`.

- **Coop's own reads of a repository's Git files no longer freeze on a named pipe.** Before each Git
  command in your repository, Coop reads a few of its Git files itself, such as `HEAD`,
  `.git/config`, `commondir` and a linked worktree's `.git` file, and its integrity checks copy the
  refs. An agent in a box can replace any of them. A named pipe there could freeze every Git command
  Coop ran, in any repository, and so could one at `.git/HEAD` in the folder you run Coop from; a
  link to an endless device made Coop read without end. Now Coop stops with an error, and a stall in
  one repository never holds up another. Coop also refuses these files when they are far larger than
  Git writes them: over 64 KiB for `HEAD`, refs and the Git folder pointers, and over 4 MiB for
  `.git/config`. The private copies Coop takes for its integrity checks and isolated forks refuse a
  `packed-refs`, `shallow` or index file over 1 GiB; `git update-index --index-version 4` writes a
  smaller index. Coop now follows a linked worktree's `.git` file only when the repository it names
  records that worktree in return. If you moved a worktree by hand, run `git worktree repair` in it.

- **Secrets stay hidden when `COOP_REPO` names a link.** When `COOP_REPO` named a link to your
  project instead of the project folder, Coop mounted the project into the box without hiding
  anything: the agent could read `.env` files, keys and every file `.coopignore` lists. Coop now
  follows the link and uses the folder it points to, as it does when you run Coop inside that
  folder, unless the link leads to your home folder or a folder above it. It refuses to launch a box
  for a project folder that is still a link it could not follow. If your `COOP_REPO` is a link or
  passes through one, Coop now keeps your project's state under the folder's real path: agents work
  in that path, so conversations started under the old path are not offered to resume, and
  `serve.ports` publish on new localhost ports. If `COOP_REPO` itself is the link, merge or remove
  your forks with `coop fork merge` or `coop fork rm` before you update: they live in
  `<link>-forks`, where the new version no longer looks, so afterwards Coop cannot find them or
  return the tasks they took to the queue. If your project has a box Dockerfile, run `coop build`
  after updating: Coop now names its image after the folder, not the link.

- **`coop init` removes a partial file only where it wrote it.** When `coop init` could not finish
  writing a file, for example because the disk was full, it looked the file's path up again to
  remove what it had written. An agent in a box could swap a folder on that path for a link just
  then, so the removal could take a file with the same name in another folder of your repository,
  such as one `.coopignore` hides from agents. Now the removal goes through the folder the file was
  written in. Swapping that folder for a named pipe while `coop init` creates a file now stops it
  with an error instead of freezing it.

- **`coop init` replaces an agent's skills link only in that agent's folder.** `coop init` points
  `.claude/skills`, `.codex/skills` and `.gemini/skills` at the project's shared skills and replaces
  a broken link it finds there. It looked the link's path up again to remove the old link, so an
  agent in a box could swap `.claude` for a link to another folder just then, and `coop init` could
  delete a file, link or empty folder named `skills` in that folder, such as `~/.claude/skills` or
  one `.coopignore` hides from agents. Now the check, the removal and the new link all go through the
  folder `coop init` checked. A link it cannot create is now reported like a file it cannot create:
  "Could not create .claude/skills: permission denied."

- **`coop models` no longer leaves your project's services running.** Coop lists an editor agent's
  models from a short-lived box, and that box also started the project's Compose services, which kept
  running after it until a later `coop loop`, `coop fork` or `coop build`. A model refresh now starts
  no services, and one under filtered networking, whose approved rules start the services they name,
  removes them when it ends.

- **An editor session removes its own services when it ends.** A `coop acp` editor session runs its
  own copy of the project's Compose services, and it left them running after it ended, along with
  the copy from before each reload, until a later `coop loop`, `coop fork` or `coop build` removed
  them. Now the session removes its services, their data volumes and network when it ends, and a
  reload removes the copy it is done with, once none of the session's boxes is left running.

- **Codex no longer copies itself into every project's home.** Unless you set an effort level or
  an API key, interactive Codex started a background server in the box. The first time, it copied
  the whole Codex install, 382 MB, into the project's Codex home, and it ran an updater that, five
  minutes in, could download and run Codex's installer and restart the server on the newest Codex,
  a version Coop has not tested. A box stops with its session, so the server never helped. Codex
  now runs without it, as it already did in `coop loop`: new homes stay small, Codex no longer
  downloads or runs its installer on its own, and sessions with an effort level or an API key no
  longer open with "Running without the shared background server". To get the space back, delete
  `~/.config/coop/agents/codex/native-homes/<name>/<repository-key>/home/packages` while no Codex
  box runs.

- **An accounts folder Coop cannot read no longer looks like a missing account.** When Coop could
  not list an agent's accounts, such as a folder without read permission, a command that named one
  (`coop claude@work`, `coop acp`, `coop fork`, `coop eval` or `coop credentials`) said the account
  did not exist and suggested `coop login`, which cannot fix that. `coop loop`, fork loops and
  `coop acp --peer` suggested it too when they found no account to run, including for the loop's
  review agents. `coop credentials` showed fewer accounts, offered a sign-in to an agent that had
  some, or called its default account missing. Now each of these says Coop could not list every
  account and names the folder and the reason. Launches, `coop acp`, `coop fork` and
  `coop credentials` then exit 1 instead of 2, and `coop eval` and `coop loop` already exited 1.
  When a review agent has no account it can run, a loop now exits 1 too, instead of 2, as it does
  for its own agent. A loop with an account it can run still runs on it, and an account that does
  not exist keeps its sign-in advice when Coop can read the folder. `coop acp`, `coop fork` and
  `coop eval` also refuse an account that does not exist with the same message as other commands.

- **Coop cleans up the helper containers and old images it left behind.** When a run was killed,
  its credential broker container stayed on the host, stopped, and Coop removed it only together
  with a box from the same run, which was usually gone already. An interrupted check of a filtered
  box image left its container behind too. Both kept old images in use, so Docker could not reclaim
  them (3.9 GB on one Mac). Now the next `coop loop`, `coop fork` or `coop build` in that repository
  removes a broker once the Coop process that started it is gone, and each image check first removes
  checks left over for more than an hour. When `coop update` rebuilds Coop's base image, or `coop
  build` rebuilds a project's filtered image, under the same name, Coop removes the image it replaced
  if no container, running or stopped, still uses it. Images left behind this way before this
  release stay until you run `docker image prune`.

- **`coop init` no longer edits a file hidden from the box.** An agent in a box can set
  `box.compose`, so it could point `coop init --services` at a file hidden from it, such as
  `config/database.yml`, and read the copy Coop writes beside a file before renaming it into place.
  Now `coop init` stops and names the file when the box cannot see the Compose file,
  `.agent/project.yaml` or `.gitignore`, including through a link or another spelling of its name,
  and leaves that file untouched. A `.gitignore` it cannot edit now stops `coop init` before it
  writes anything. Point `box.compose` at a file the box can see, or stop hiding the file in
  `.coopignore`. `coop init` also stops, and names the `.coopignore`, when a `.coopignore` on the
  way to one of these files is a link, a folder, unreadable or over 1 MiB.

- **A running box keeps working when a sign-in renewal fails for a few minutes.** Coop renewed an
  agent's sign-in during a run only two minutes before its access expired. When the provider did not
  answer, answered with a server error or a rate limit, or another Coop command was using the
  account, the box lost access to the provider until it restarted. Now Coop starts renewing ten
  minutes before the access expires, so a box rides out a provider outage of up to about nine
  minutes. When a renewal fails that way, the box keeps the access it has, and Coop tries again, at
  most a minute apart, while that access lasts. Boxes that share an account take turns, so the
  provider is asked once per try rather than once per box. The box still loses access when the
  provider refuses the saved sign-in, when you sign in again or remove the account, when its access
  expires before a renewal gets through, or when another command keeps the account busy for more
  than about two minutes.

- **The daily cleanup no longer removes files that boxes on another Docker daemon use.** It asks
  Docker which files boxes use, but only the current daemon answers. After `docker context use`, or
  with `DOCKER_HOST` pointing elsewhere, boxes still running on the first daemon looked unused, and
  the cleanup removed their run files once they were an hour old. Each run now notes which daemon
  its box started on, and the cleanup removes a run's files only when that daemon says no box uses
  them. Some run folders are therefore never cleaned up, and they can hold credentials. With Docker
  reached over TCP or SSH, the cleanup now keeps every run's folder; earlier versions cleaned them
  up. It also keeps folders from earlier versions, which name no daemon; the folder of a run killed
  before Coop checked that Docker was running, or whose Docker did not answer just before its box
  started; and folders of a Docker engine you reset or stopped using (resetting Docker Desktop or
  OrbStack gives the engine a new identity). The cleanup runs once a day and checks only the engine
  that is current when the day's first box starts, so a second engine that is never current then
  keeps its folders too. While no Coop is running, you can delete the `coop-run-*` folders in
  `~/.config/coop/runfiles` yourself. Box files that versions before 10.1.2 left in the temp folder
  stay too.

- **An interrupted credential renewal no longer locks the account.** When a renewal of an agent's
  sign-in was cut short by Ctrl-C, a lost connection or a provider error, Coop refused that account
  until `coop login`. Now it finishes the renewal from the provider's answer when it got one, and
  otherwise tries the renewal again. Only an answer that refuses the saved sign-in, or one Coop kept
  but cannot use, asks for `coop login`. An account or agent home whose first setup was interrupted
  is set up again instead of asking you to restore a backup.

- **`coop init` no longer edits a `.gitignore` link.** An agent in a box can change files in your
  repository, so it could replace `.gitignore` with a link to a file such as `~/.zshrc`, and the
  next `coop init` added Coop's ignore lines to that file. Now `coop init` stops and says so when
  `.gitignore` is a link or anything else that is not a regular file, and leaves both the link and
  the file it points to unchanged. Replace the link with a regular file, then run `coop init` again.

- **An interrupted first launch no longer leaves an agent home refusing every launch.** Coop wrote
  an account's sign-in markers into its agent home before noting that it had, so a launch cut short
  in between made every later launch refuse the home as holding unmanaged credentials. Coop now notes
  the setup first and finishes it at the next launch, and puts back a marker missing from the home.

- **`coop usage` says partial when it cannot list some history.** An account or history folder
  Coop could not list, such as one without read permission, dropped out of the totals without a
  word. Now that provider's usage says it is partial and names the folder it could not list, and
  when that hid accounts, `coop usage <provider>@<account>` says so instead of saying the account
  does not exist.

- **Coop removes the services killed loops and editor sessions leave behind.** A loop or editor
  session runs its own copy of the project's Compose services, and only a loop that ended cleanly
  removed them, so every killed loop and every editor session left its containers, data volumes and
  network behind (13 such projects on one Mac). Coop now records which of its processes owns them.
  Once that process is gone, the next `coop loop`, `coop fork` or `coop build` in that repository
  removes them, and `coop fork rm` does the same for the fork's own runs. Services started by older
  versions carry no owner; remove those with `docker compose -p <project> down -v`.

- **Docker-excluded source edits no longer block filtered editor sessions.** Project image builds
  and startup checks now honor the configured Dockerfile's `.dockerignore`, or the root file when
  there is no specific one. Excluded files are omitted from both the build and its fingerprint;
  Dockerfile, ignore rules and included build inputs still require an explicit rebuild when they
  change. Coop's own toolchain image excludes runtime source, so other tasks can keep editing while
  Zed connects. Run `coop build --egress filtered` once to prepare that narrower context.

- **Coop cleans up its git views of deleted repositories.** Coop keeps a small trusted view of each
  repository it runs git in, and never removed one, so every evaluation, session and temporary
  repository left its view behind (about 14,000 on one Mac, almost all for repositories long gone).
  The daily cleanup now removes the views of repositories that no longer exist, and keeps one that
  holds an unfinished rebase or merge; a view that is needed again is rebuilt.

- **Check a worker's session data before you upgrade it.** A new build can convert a worker's
  session data when it first opens it, and the build before it can't open converted data. Stop
  the worker and run `coop sessions check` with the new build: it says whether that build uses
  the data as it is (exit 0), will convert it (exit 3), or can't use it (exit 4), and it changes
  nothing. A build that can't open session data now names the data's schema and the schemas it
  supports. Old data stays convertible for one upgrade step; when a later release stops
  converting some, its notes say which build converts it first.

- **Agent starts no longer reread history from before v11.** Each start read and checksummed the
  transcripts left in the old account and editor folders to find any not yet imported, and
  `coop acp` did so for every provider. Coop now remembers when an import finished and skips that
  work while the old folders are unchanged, which it checks from file names, sizes and change times
  alone. New history in an old folder is still imported. On one install the check before a Codex
  start fell from about 0.46 s to 0.11 s, and before a Claude start from 0.34 s to 0.02 s.

- **Coop keeps finished network runs for 90 days.** Every filtered run left a record in Coop's state
  folder forever, and every filtered launch and loop start re-read all of them, so launches slowed
  down over time and, past 10,000 files, interrupted runs would have stopped being cleaned up. Coop
  now removes a finished run's record 90 days after the run ended, and keeps a run still waiting for
  cleanup until that cleanup is done. `coop net runs --all` shows the runs from the last 90 days.
  The copy of a launch's network rules goes too, once no kept run uses it and no launch has used it
  for 90 days, so an editor session that starts no box for that long may have to be started again.
  `coop net setup`, and a filtered launch that sets the host up first, test filtered networking with
  a short run, and each such check left a copy of its test rules for good (92 on one Mac). That copy
  now goes once the check's record does, including copies earlier versions made, because Coop wrote
  in each one that setup made it. Other copies made by earlier versions are kept.

- **Run `coop net setup` once after upgrading.** This release changes both images that filtered runs
  use, the gateway and the client image, so a network setup made by an earlier version no longer
  applies. Until you run it, a worker refuses filtered sessions, and the next filtered run in a
  terminal or an editor sets networking up on its own first, which can take several minutes. Worker
  sessions created before the upgrade keep their old setup, so start them again. A filtered project
  with its own `.agent/Dockerfile` also needs `coop build --egress filtered` once.

- **Box images are about 41 MB smaller.** They no longer include Staticcheck and govulncheck, which
  Coop built in for its own checks, or the `jv` JSON Schema validator, so a build also no longer
  pulls a Go image to compile them. If your project or its agents use one of these in the box,
  install it in your project's `.agent/Dockerfile`.

- **Network setup keeps working after Ubuntu updates.** Coop builds its network gateway image with
  nine Ubuntu packages at exact versions. Three of them (nftables and the libraries it uses) come
  from Ubuntu's updates archive, which keeps only the newest version, so the next nftables or
  iptables update would have broken the build, and `coop net setup` with it. The build now installs
  the same versions from a dated snapshot of the archive on `snapshot.ubuntu.com`, which never
  changes.

- **Fewer upgrades ask you to run `coop net setup` again.** The network gateway image changed
  whenever Coop updated any library it uses, and a changed gateway image needs a new network setup.
  The gateway now builds from only the libraries it needs itself, so a release that leaves those
  libraries alone keeps your network setup.

- **`coop tasks watch --json` no longer reports `starting` for forks.** A background fork loop
  counts as running from the moment `coop fork <name> -d` returns, so the field, which was true
  only while a start was in progress, is gone, and so is the board's `starting` state. An
  interrupted start no longer leaves a record behind for `coop fork stop` to clean up. If an
  interrupted start or a failed stop from Coop 11.3.0 or earlier left one, `coop fork` refuses it
  and prints how to clear it: stop any loop of that fork still running in this repository, remove
  the file, then run `coop fork stop <name>`.

- **`coop usage` removes cached records of deleted transcripts.** The usage cache kept a record for
  every transcript it ever read, so it grew as Claude deleted month-old transcripts and as accounts
  and history folders were removed. Now `coop usage` without a provider, once it has read all
  history, deletes the records it no longer needs. A one-provider check, an interrupted check or a
  check that could not list every history folder deletes nothing, and neither does a history folder
  Coop could only partly read. Coop removes only its own record files.

- **Signing keeps the IDs of signed and pushed commits.** Coop signs the commits agents make in a
  box after ordinary agent runs, after each loop cycle, at the end of a loop run, and with
  `coop sign`. Each pass used to rewrite every commit in its range, signed ones too, so a commit
  another agent made in the same checkout, or one you had already pushed, got a new ID, and a
  pushed branch then needed a force push. Signing now starts at the first unsigned commit, and the
  signed commits before it keep their IDs. It never rewrites a commit the branch's upstream already
  has: when the first unsigned commit is already pushed, Coop stops and says so, and `coop sign`
  then signs only the commits you have not pushed. When every commit is already signed, `coop sign`
  leaves them alone and says so. A signed merge below the commits to sign, such as one a mid-run
  `git pull` made, no longer stops signing.

- **The daily cleanup of what killed runs leave behind now finishes.** Coop marked the day's
  cleanup done before starting it, so a quick first command such as `coop tasks ls` cut it short
  and everything waited another day. A box that went away while the cleanup checked it also
  cancelled the whole pass. The cleanup now starts alongside commands that run boxes, such as
  `coop claude`, `coop acp` and `coop loop`, counts only once it has finished, and skips a box
  that is already gone. A pass that could not ask Docker which files boxes use does not count, so
  the next box tries again. It also removes the Docker and Compose client folders killed runs
  leave behind. Coop now keeps each run's Docker client folder in its own private folder, as it
  already did the Compose one, because removing one from a shared /tmp is not safe. The cleanup
  leaves the ones earlier versions put in the temp folder, possibly thousands. While no Coop is
  running, you can delete the `coop-docker-client-*` folders in `$TMPDIR` (or `/tmp`) yourself.
  The cleanup still removes only Coop's own entries that are over an hour old and that no box uses.

- **`coop tasks` points out bulk left in the task queue.** Agents sometimes kept compiled
  programs, release tarballs and toolchains in task folders, and left release files next to the
  state folders, where they stayed until someone deleted them. `coop tasks done`, `coop loop` and
  `coop fork merge` now warn when a task they complete keeps a compiled program or more than 25 MB
  in its folder, and an agent that completes its task in the loop is told the same. `coop tasks`
  warns about any file or folder in `.agent/tasks` besides its README, state folders and backlog.
  Hidden files are left alone, and neither warning deletes anything or stops the command. A new
  project's tasks README and the loop's instructions to agents say what to keep instead: the
  commit, sha256 and build command.

- **Box images check asdf before installing it.** Coop's box images download asdf, the tool that
  installs the versions in a project's `.tool-versions`. A build now compares that download with the
  checksum Coop pins for its architecture and stops if they differ, so a changed or tampered
  download never reaches a box.

- **Coop no longer runs your `sqlite3` on Codex's log database.** Before launching Codex, Coop used
  whatever `sqlite3` was on your PATH to add a trigger that stopped Codex writing its feedback log.
  Since 5.0 that database lives inside each box and is removed with it, and the Codex that Coop
  ships trims the log itself, so the trigger only touched an old copy in your credential home.

- **Preset roles move on when a provider reports a rate limit with single quotes.** Inside the box,
  `coop-consult` and `coop-delegate` missed a limit written as `{'status': 429}` or `{'code': 429}`,
  so a role could retry or stop on the limited target instead of moving to its next one. The in-box
  check now uses the same patterns as Coop's own rate-limit detection.

- **`coop approve` no longer labels each rule.** The rule diff no longer repeats "already approved",
  "new request" or "no longer requested" after every rule. The `+` and `-` markers say it, and the
  `NETWORK RULES` heading explains the ones it uses, so the meaning survives without color.
  `coop net` keeps its labels: there they tell a requested rule from an approved one.

- **A running editor session picks up the network rules you approve.** Before, `coop approve`
  changed only new runs, so a filtered `coop acp` session kept the old rules until the editor
  reconnected. Now the session picks up the change once no prompt is running: it starts a new box
  under the newly approved rules and only then stops the old one, so the conversation continues,
  and the editor shows one line saying so. A running box's own rules never change. If the approval
  can't be applied, for example because the project file changed again, or the new box stops before
  the switch, the session keeps the rules it had and says why. After `coop net forget`, a running
  session keeps its rules until you reconnect the editor. `coop approve` now asks to approve access
  "for new runs and running editor sessions".

## 11.3.0

- **Usage includes recent requests in large histories.** Old transcripts no longer fill a scan
  budget that excludes newer usage. Coop caches parsed token records from unchanged files,
  rereads changed files and recovers later requests after an oversized JSONL record. The cache
  contains no conversation text; estimates still use the rolling 30-day window and current rates.
  GPT-6 Sol, GPT-5.6 Terra and GPT-5.6 Luna now have their documented API tariffs.

- **A worker removes the repository copies no session can still use.** A worker staged a copy of
  every repository a job named and never removed one, so a controller that started sessions
  often filled the worker's disk: on one install, about 3 GB an hour. Each maintenance pass now
  removes copies no open or closed session uses. It keeps each repository's newest copy and its
  first download, a task session's own copy for 30 days, and any copy used in the last hour.

- **A slow repository fetch no longer holds up the worker.** A worker runs one command at a time,
  and a session create waited for every repository it had to fetch: on one install a create took
  18 minutes, and every other session's work waited behind it. Every new copy a create needs now
  starts fetching in the background at once, its grant included, and the create waits for them
  three seconds in all. It is then tried again until its repositories are ready, every 15 seconds
  at most, while the worker runs every other command. A repository with submodules does too. Only a
  repository's first copy asks for its grant first, and its download then runs in the background.
  Finished downloads release their bookkeeping before the worker reuses the copy.

- **A branch that moved on no longer refuses its job.** A worker staged a job's repository only
  if the branch still pointed at the job's commit, so a create failed whenever the branch moved
  while it was being set up, and a task's next session failed for good once its main branch had
  moved. The commit now only has to be on the branch: its head or in its history. A branch
  rewritten past it, by a force push, is still refused.

## 11.2.0

- **Task tools shut down cleanly.** A helper's normal exit no longer loses its final output
  or reports a false shutdown error.

- **Invalid native account access stays stopped.** Another request cannot slip through
  while the first account refusal is being cleaned up.

- **Filtered runs start with separately isolated helper clocks.** Coop now verifies the
  helpers' actual clock offsets instead of requiring matching namespace labels. Network
  restrictions, credential protection and expiry checks stay in place.

- **Usage includes native CLI history outside Coop.** Supported Claude, Codex, Gemini and Grok
  histories on this computer now contribute to recorded estimates, with copied requests counted
  once and unmatched accounts kept unassigned. Optional host-only provider reports show API
  organization/team spend, or selected Google billing SKUs, separately from local estimates.
  Reporting dates, scope, partial coverage and failures remain visible; these are not personal
  subscription bills.

- **Usage makes capacity and estimates easier to read.** Claude and Codex show reset counts and
  reported expiry without redeeming them. Credit balances use readable digit grouping, reset
  times are quieter and dot-free, and account estimates show their `/30d` period. Provider
  subtotals and a highlighted total sum recorded API-value estimates, with incomplete coverage
  marked partial; these are not credit balances or bills.

- **A turn that goes quiet after its tools stops.** A model that sends nothing for 15 minutes
  after its tools answered used to hold its turn until the turn deadline, while its provider kept
  pulsing. The turn now fails with `acp_quiet`, which a caller can retry.

- **An early exit says what kind of exit it was.** When a model process closed before its
  response, the turn only said "ACP child closed before its response". It now also says how the
  process ended and what its stderr looked like: line and byte counts, a digest that groups
  identical exits, and word classes such as rate limit or authentication. It never includes the
  stderr text.

- **Filtered networking works where 1.1.1.1 is intercepted.** The network gateway's resolver
  now dials both of Cloudflare's addresses, 1.1.1.1 and 1.0.0.1, at once and uses the first that
  proves its certificate, so an address that is blocked, hangs, or answers with another
  certificate no longer stops name resolution. Both must present cloudflare-dns.com; nothing falls
  back to an unverified resolver.

- **A quiet turn's pulse says what the provider is still sending.** `provider.alive` now carries
  `last_kind`, the latest frame's method or session update type. It never carries frame content.

- **A reservation released mid-scan no longer fails a session create.** When another session
  released its workspace reservation while Coop was reading the registry, the missing file was
  reported as a registry problem and the unrelated create failed with "no such file or
  directory". A released reservation now counts as absent; an entry that is still there but
  unreadable is still refused.

- **Two project reviews starting together no longer fail on their shared lock.** Creating the
  review lock could fail with "no such file or directory" when another review created it at the
  same moment. Coop now tries that open again.

- **Known issue in filtered Claude sessions.** An intermittent proxy disconnect remains
  under investigation. The networking fixes above do not claim to repair that earlier failure.

## 11.1.0

- **Updates prepare the image your project actually uses.** After installing a new release,
  Coop uses that version's image definitions and the project's effective network mode.
  Filtered projects no longer finish updating with only an unused ordinary image built.
  When upgrading from v11.0.2 or earlier, run `coop update --box-only` once using the newly
  installed binary; the older updater cannot use the new preparation flow.

- **Build and continue from an interactive launch.** When a filtered project needs a build,
  Coop offers the next step before slow launch setup. Review the saved build snapshot and
  approve it once; unattended runs still require an explicit host build. Later edits are not
  approved, and project builds remain on the selected Docker daemon.

- **Image checks distinguish missing builds from Docker failures.** Missing or changed project
  builds are explained early. Docker observation failures retain their real cause, and
  `update --check` no longer reports ordinary-image records for filtered projects.

- **Warm sessions keep their full idle interval.** Slow startup or a long model turn no longer
  consumes the session's next idle interval. Broker credentials still renew continuously and
  expire closed if renewal stops.

- **Native proxy disconnections are easier to diagnose.** Helpers record a safe failure
  category when protected traffic stops, without logging account details or credentials.

- **Reviewed commits say what they change.** A review request may name the commit message
  (`commit_message`, up to 4,096 bytes); the published commit carries it instead of "Reviewed
  changes", still followed by the `Coop-Review` trailer. Workers advertise
  `review-commit-message:1` once their daemon accepts the field.

- **MCP credential brokers start under strict file permissions.** The broker's configuration
  and secret files are written with their modes set, so a private umask no longer leaves them
  unreadable to the broker's own user.

- **A parked session is not refused while Coop checks it.** Restore, checkpoint, task binding
  and review wait up to ten seconds for a session runtime that Coop's own cleanup briefly holds,
  instead of refusing as if a turn were running. A session with a turn running or queued is
  still refused at once.

## 11.0.2

- **Providers start under strict file permissions.** Native provider helpers now receive
  readable public configuration when the host uses a private umask. Secret custody remains
  owner-only and outside the provider box.

- **Provider sessions recover from brief credential-file read failures.** Transient
  host-to-VM reads can recover within the existing load deadline, instead of stopping after
  three reads. Invalid accounts still fail closed.

- **Task bookkeeping stays out of code changes.** Verified controller-managed task files
  no longer appear as unfinished product changes, including after checkpoint recovery.
  Actual code changes and unrelated files remain visible.

- **Credential completion fills in accounts and actions.** Shell completion now fills in
  account names and actions for `coop credentials`.

## 11.0.1

- **Codex starts with the selected workspace.** Native account discovery now matches the
  public broker identity while preserving upstream workspace routing and sibling accounts.

- **Claude loads managed settings and organization policy normally.** The broker admits exact
  native policy requests for subscription and API-key accounts, eliminating false access-denied
  warnings without changing genuine upstream policy or authentication errors.

- **Provider sessions survive transient VM file reads.** Credential leases reopen briefly
  incomplete or missing atomic publications instead of permanently disconnecting a healthy
  session. Revoked, expired and invalid accounts still stop protected traffic.

- **Slow activity reads no longer make worker polls look stale.** Workers refresh the outgoing
  timestamp before sending, so delayed local reads do not cause clock-skew refusals.

- **Fleet reads stay responsive during slow commands.** Workers can inspect sessions and
  operations while another command is still running, with bounded concurrency and ordered writes.

- **Stopping services cleans up filtered networks.** `coop down` now removes unused
  project networks left by filtered runs, while keeping attached networks and stored data.

- **Same-provider peers work independently.** Naming the lead's provider with `--peer` now
  mounts and authorizes its consult wrapper while preserving the lead's model and effort,
  including in editor sessions. The peer shares the lead's selected account.

- **Status checks no longer interrupt fork startup.** Concurrent Git refreshes now keep their
  temporary files separate. Review setup also reports the real Git error instead of branch loss.

- **Background fork stop finishes cleanup.** Stopping a loop now retires its dead sandbox records
  as well as its containers, so stale cleanup state does not block later work. Paused tasks remain.

- **Editor switches give useful retry guidance.** A request during an agent switch now asks you
  to wait and retry instead of incorrectly suggesting that the thread is unavailable.

- **Editor sessions load shared MCP tools.** The editor connection now receives the same
  validated tool configuration as other Coop runs, including after a restart. Conflicting
  editor and shared tool names produce a setup error instead of silently replacing a server.
  Servers with no arguments, environment variables or headers are no longer silently dropped.

- **Unsupported Haiku effort is caught before startup.** Haiku 4.5 has no reasoning-effort
  control; use `claude:haiku` without a suffix. Coop now explains this instead of silently
  dropping the requested level in the CLI or failing the editor's first message.

- **Model discovery respects project networking.** Claude and Gemini catalog refreshes now use
  normal host admission for the selected account, including filtered projects. Failed refreshes
  retain the last good list and explain setup failures without exposing provider diagnostics.

- **Grok models use your Coop account.** Catalog discovery no longer requires a separate host
  Grok login or host CLI; it asks the boxed client using the same selected account as a Grok run.

- **Existing Grok sign-ins migrate safely.** Coop accepts normal native writer-lock permissions
  while preserving the lock, excluding active writers and keeping credential storage private.

- **Credential startup checks scale to busy Docker hosts.** Complete mount inventory uses
  bounded batches while preserving daemon identity, read-only mounts and stopped-container
  safety checks; unavailable or incomplete observations still refuse startup.

- **Retired test containers no longer block unrelated sign-ins.** Removed temporary bind paths
  on stopped containers are compared safely; credential overlaps, unresolved live mounts and
  ambiguous symlinks still prevent migration.

- **Native accounts start in filtered projects.** Generated native-home and public broker-helper
  mounts now pass the same custody checks as other binds, preserving quoted paths and refusing
  missing sources or unsafe overlaps.

- **Model discovery allows normal box startup.** Native account setup no longer consumes the
  catalog request deadline. Both startup and catalog requests remain bounded and cancellable.

- **Grok catalog refresh initializes its broker.** Discovery uses the native external helper
  before requesting models, without device sign-in or exposing account tokens to the box.

## 11.0.0

- **Repository-local native history and host-only provider credentials.** Each project keeps its
  own complete provider home, including native resume, settings and history indexes. Older
  transcripts import only when their repository ownership is known; originals remain intact and
  deleted sessions are not resurrected. Editor homes survive account switches. A run-local broker
  serves API-key and subscription accounts while the host owns renewal; real provider grants no
  longer enter coding boxes. Online provider runs require Docker. See `MIGRATING.md` before reusing
  existing accounts, and manage sign-in/removal through the host.

- **Patched Go networking.** Builds and box helpers use Go 1.27.2 and the updated networking
  library, addressing the October Go security advisories.

- **Optional isolated-write forks.** `coop fork <name> <target> --isolated` gives native Git its
  own metadata/objects and fences parent checkout/Git stores from box and service mounts.
  Review uses exact private committed custody; checks run on a separate clone. Publication
  rejects unsafe intermediate history without a force bypass and retains a durable candidate
  and forward-reconciliation journal after interruption. Shared-write defaults stay unchanged.

- **Filtered networking works with Docker 27 workers.** Setup could pass its network checks but
  fail to stop its helpers because Docker 27 calls the stop option `--time`, not `--timeout`.
  Cleanup now uses the portable `-t` flag and still checks that the exact workload stopped.
  Filtered repository-read-only worker jobs also keep their separate output mount writable without
  an unsupported mount spelling. Worker docs explain CLI, Buildx and shared-path requirements.

- **Filtered launches work after `coop up`.** A filtered box launched after `coop up` failed with
  "container … is not connected to the network …_filtered": Compose rebuilt the filtered services
  network and could not detach a service that `coop up` had moved off it. The launch now declares
  that network as it already is, so Compose only recreates the containers, and services keep their
  data, anonymous volumes included. A failed Compose step now ends with Compose's own reason.

- **A second landing page: put your agents to work while you sleep.** `coop.dryga.com/orchestrator.html`
  is for teams that want agents working through many tasks unattended; the homepage stays the
  sandbox story. A pinned `coop loop` board lives through one night, 22:00 to 07:00, beside seven
  steps: tasks queue up, each gets a fresh agent, is checked and committed, accounts rotate at
  their limits, a question waits while the work goes on, a final review sends one task back, and
  morning shows 120 done and one decision. Every page's menu links it as Orchestrator, and the
  homepage's Task queue leads to it. Its figures are its own, each checked in its source.

- **Final review counts only its own pending reviews.** `coop loop` refuses to load more than
  4,096 pending final-review records, and it counted every repository's records on the machine,
  not just the queues it was loading, so enough of them elsewhere would have stopped every final
  review. It now counts only the selected queues' records.

- **Boxes start faster in a cluttered checkout.** Before every start, co:op checks each path in
  the checkout, gitignored task folders included, against 64 secret-file patterns, so it can hide
  the matches. With 40,000 paths that took about half a second. Two string comparisons now rule out
  almost every name before the full pattern match, with the same answers, so the check takes about
  70 ms. In co:op's own checkout, a filtered start went from 3.18 s to 2.62 s, against 2.30 s for
  a clean clone.

- **Filtered boxes start faster.** A filtered launch checks about 74 times that Docker is still
  the daemon it bound to. Each check started a `docker info` process (about 40 ms on a busy Mac);
  co:op now sends that one request over the same socket itself (about 4 ms), and still checks
  every time. On a busy Mac (load 9 to 15), the median filtered start went from 3.75 s to 2.89 s.

- **`coop tasks watch` sits idle quietly.** The board re-read every task's records, done ones
  included, on each 400 ms tick: on a 650-task queue that kept a fifth of a CPU busy while nothing
  changed. It now reads again only when a task folder or a listed task's files change, and at
  least every 10 seconds; leases are still checked every tick. Measured on this repo: 21% CPU
  before, about 1% after.

- **Workers keep the checkout path to themselves.** What a worker tells its controller about a
  turn (tool titles, commands and their output, thoughts, plans) still named the session's checkout
  by its absolute path, though the docs said the worker removes it: the worker looked for the path
  in a field the session service never filled. The service now sends it with every narrated
  event, so the controller sees `lib/a.go` where it used to see `/Users/you/…/lib/a.go`.

- **Session workspace and checkpoint calls check their headers.** `POST /v1/sessions/{id}/workspace`
  and `/checkpoint` now answer `400` to a query string, a Content-Type other than
  `application/json` or a repeated `Idempotency-Key`, like every other session mutation. Before,
  such a request reached the service. Workers already send these headers.

- **Park a decision for later.** `coop tasks park <id> ["<reason>"]` answers a blocked task with
  "not now": it moves to the backlog with its decision still open, so `coop loop --preflight` never
  re-queues it. Press `:b` in `coop tasks decisions -i` to do the same. `coop backlog promote`
  brings a parked task back blocked, with its question.

- **Docs in plain words.** The README is now a short introduction with the install, a quickstart and
  a tour of each feature. The detail it used to carry moved to reference pages under `docs/`:
  the box, agents and accounts, forks, the loop, editors and MCP, evaluations and configuration.
  Contributor guidance moved to `CONTRIBUTING.md`, and the guide's troubleshooting table now has
  every row.

- **Current models in the starter preset.** `coop presets init` now writes Opus 5.5 at xhigh as
  the lead, falling back to GPT-6 Astra; Fable 5.1 at max effort as the thinker; GPT-6 Astra as the
  critic; and Gemini 3.8 Flash for fast edits. Help, docs and the `coop models` examples name
  current models too. Presets you already have keep their models.

- **Simpler usage output.** Keep progress bars and show limits before the 30-day API estimate,
  without default tags or repetitive footnotes. Bars align across accounts, shared editor usage
  has a clearer label, and each provider's columns fit its own labels. Percentages right-align so
  resets line up, balances and the dimmed 30-day `Σ≈$…` totals start where `100%` does, totals
  appear only when usage has API pricing, and API-key or Vertex sign-ins without limits fit on
  one line, such as `personal (API key)  Σ≈$2.62`. Resets show the time left and the moment in
  UTC, such as `resets in 2d 2h (Oct 4, 08:35 UTC)`. Select one account for history and pricing
  details.

- **Clearer installer output.** Installation, optional shell setup and sandbox checks use
  separate readable blocks, with final examples for starting an agent and finding help.

- **Clearer fork examples.** Fork help calls its example fork `myfork`, so
  `coop fork myfork claude` no longer reads like a sign-in command.

- **Fork editor sessions start without a target.** `coop fork myfork acp` starts the agent the
  fork was created with, so an editor entry needs only the fork's name; choose the model and
  effort in your editor. A target such as `claude:opus` still picks the agent.

- **Loop report grammar.** A task the final review reopened once reads "was reopened once by the
  review", not "was reopened 1 times".

- **Plainer claims.** The help header, README and docs describe the box as a sandbox that holds
  only your project, not one an agent "can't escape", and link what the agent can still reach.

- **A commented `.coopignore` entry is flagged.** A launch warns when an entry has a comment after
  its pattern (`prod.yml  # note`): only a line that starts with `#` is a comment, so such an entry
  is one pattern that hides nothing. The README example puts its comments on their own lines.

- **Claude's MCP logs stay with their run.** Every box shares one `~/.cache` volume across
  projects and accounts, and Claude Code kept per-project MCP logs (tool traffic included) in it,
  where any other box could read them. Each run now gets its own empty
  `~/.cache/claude-cli-nodejs`; the rest of the cache stays shared.

- **`coop init` warns on Apple container.** Filtered networking, which `coop init` turns on,
  needs Docker. On Apple `container`, init now says to start or install Docker; online agents
  require it even with open networking. The project file stays filtered, so
  teammates on Docker keep the filter.

- **Agent help lists the network options.** `coop help claude` (and codex, gemini, grok) now
  shows `--egress`, `--allow-domain` and `--egress-rules`, which every launch accepts, so you can
  find how to let one run reach a package registry.

- **Filtered boxes tell agents where toolchains come from.** The note an agent gets inside a
  filtered box no longer promises that `.tool-versions` is installed on start, which only open
  boxes do. It says a pinned toolchain comes from the project's `.agent/Dockerfile`, which
  `coop init --stack asdf` writes from `.tool-versions`.

## 10.1.2

Changes since v8.1.0 include provider-client updates, `coop usage`, and the new controller-worker
workflow. Follow the [upgrade guide](MIGRATING.md#v1012-upgrading-from-v810), including the
session-state backup, before upgrading.
[Compare the public upgrade](https://github.com/AndrewDryga/coop/compare/v8.1.0...v10.1.2).

- **Fix large worker result delivery.** Results containing Unicode separators stay within
  transmission limits on Go 1.27.

- **Refresh dependencies and client compatibility.** Update Go/tooling, CI actions, container
  bases, release tools and locked provider clients. Patched npm transitives remove the reported
  fast-uri and ip-address advisories. Codex ACP2 uses its current versioned phase metadata and
  terminal output stream. Gemini's effort and sign-in settings are image-owned system files,
  satisfying its ownership checks without shared settings mutation or host ownership changes.
  Codex workspace authentication refusals advance configured account fallback. Native Claude
  startup verifies the selected account/trust config across VM mounts before launching.

- **Retain typed terminal exit evidence for controllers.** Command output deltas and numeric
  exit codes/signals survive output truncation and pass through bounded controller activity.

- **Inspect limits and recorded token value with `coop usage`.** Read all configured providers
  or one `provider@credential`, preserving independent failures and local reset times. Native
  histories produce a deduplicated rolling 30-day estimate at current standard API token prices,
  including subscription activity. Missing prices, partial records and unknown shared-editor
  credential attribution stay explicit; lossy retained turn aggregates are never added to it.

- **Preserve multiline instructions when freezing eval suites.** Authored instructions retain
  exact leading newlines and indentation in reloadable frozen manifests.

- **Add trusted runtime profiles for agent evals.** Custom cases can bind clean external
  build inputs to an explicitly approved immutable image, `/app`, fixed CPU/memory/PID caps
  and separate candidate/verifier budgets. Original profile identity and content remain
  authoritative; retained copies cannot approve builds. Candidate networking stays provider-only
  filtered and grading stays offline. Reports disclose the declared but unenforced/unmeasured
  storage limit; these profiles do not reproduce official benchmark environments.

- **Deliver ACP cancellation before teardown.** A prompt interrupted just after delivery still
  receives a bounded cancellation attempt. Successful cancellation gets a short EOF-drain window
  before forced termination, retaining larger restricted/filtered cleanup allowances.

- **Stop admitting worker commands after shutdown.** Cancellation racing a successful controller
  poll no longer starts another source/body download or recreates an acknowledged command receipt.
  Successful response acknowledgements and in-flight cleanup retain their existing behavior.

- **Breaking: upgrade configuration and controller contracts together.** `coop.conf` rejects
  unknown, duplicate and retired keys; move retired loop settings into `.agent/loop.yaml`.
  Podman and `COOP_AGENT_PACKAGES` are no longer supported. `coop tasks clear`, `split`
  and `flags`, `coop fleet`, `coop fusion`, `coop worker`, `coop sessions serve` and `policies`
  are removed without aliases. Use presets/peers and direct detached fork loops for composition.
  Use `coop tasks rm --all-done`, the canonical shared queue and the connection command below.
  `coop approve` replaces `coop net approve`; `coop net blocked` replaces `net explain`.
  Back up session state before its schema-25 upgrade; older binaries cannot reopen the new root.
  See [MIGRATING.md](MIGRATING.md) for required actions and preserved data.

- **One outbound worker connection, generic controller-owned jobs.**
  `coop sessions connect --controller <https-url> --token-file <path>` owns enrollment and
  its private local service; optional `--state` and `--ca-file` select storage and trust.
  There is no worker JSON configuration, local execution-policy catalog or separate serve command.
  Protocol-v2 jobs freeze exact source, targets, execution/network settings, work/check environments,
  literal review argv and finite CPU, memory and PID caps. Controllers require live `job-setup:2`
  proof. Historical evidence remains readable; unsupported old execution authority is not invented.
  Provider accounts still need normal worker authentication: controller-supplied model credential
  exchange is not part of this release.

- **Structured completion and caller validation survive interruption.** Remote turns can bind
  a JSON Schema and its exact digest, repair invalid output in the same native session, and
  retain schema-valid candidates for explicit caller acceptance or rejection. Only accepted,
  digest-bound bytes publish; repair/review budgets are bounded. Pending candidates and durable
  decision receipts survive restart, while retryable runtime cleanup cannot silently decide them.

- **Complete source custody and controller publication.** Workers fetch pinned GitHub source
  with repository-scoped host grants, verified LFS objects and explicitly authorized recursive
  submodules; repository-controlled URLs cannot redirect those grants. Checkpoint transfer/restore,
  acknowledged activity and command receipts survive bounded retries and restarts. Controllers can
  request reviewed changes be published to a draft pull request; large bundles travel separately
  from control messages. Storage admission distinguishes protected, disposable and unknown usage;
  its watermarks are not per-workspace disk quotas.

- **Review uses the captured parent and frozen job setup.** Controller gates retain accepted
  command, environment, networking and resource limits, ignoring repository box policy and ambient
  worker launch settings. Prepared candidates now name their trusted parent for `COOP_REVIEW_BASE`,
  so gated reviews can start. Stale or mutated evidence cannot become publishable. Green and red
  gates retain stdout/stderr for paged controller reads and useful startup errors; the 64 MiB
  retention cap explicitly reports truncation.

- **Worker progress and usage are more accurate.** Completed commands report on the next poll
  immediately; restarted workers promptly prove surviving runtimes before advertising capacity.
  Controller-session discard does not run repository Compose teardown. Codex whole-turn accounting
  reads cumulative native evidence when available instead of counting only the last model call.
  Controller Codex disables dynamic plugin additions, and generated-file guidance follows the
  stable caller prompt to improve cache reuse without altering unrelated host features.

- **Network isolation and recovery remain run-scoped.** Admitted modes and captures survive loop,
  fork, editor and controller execution. Kernel-protected host ranges and gateway lifecycles preserve
  unrelated concurrent boxes; active streams are not limited by admission timeouts. Ownership-checked
  recovery never silently widens access. Inspection distinguishes blocked destinations, direct
  gateway refusals, unknown observations and incomplete cleanup.

- **Credential and MCP brokering cover supported paths.** Supported provider API keys remain
  host-held behind per-run stand-ins under filtered networking, including peers, roles and fallbacks.
  Supported remote MCP credentials are brokered in open and filtered runs, including read-only
  sessions; unsupported shapes are identified explicitly. Offline runs omit remote MCP servers.
  Access-only credentials renew on the trusted host, while native sign-in storage retains each
  provider's refresh semantics.

- **Locked clients, explicit builds and worker-managed images.** Coop-managed images use pinned provider
  CLIs/adapters with native updaters disabled. Restricted project images require explicit reviewed
  host builds and immutable input/client-base proof. Managed image identities avoid cross-version
  replacement and support scoped cleanup. Controller jobs run the worker's base, never a job
  repository's Dockerfile. Project tool-version pins also apply from temporary directories.

- **Host, service and filesystem authority are stronger.** Compose uses private validated
  configuration, approved scope and exact-owned cleanup. Secret-file and custom-volume approvals
  bind actual storage authority; read-only sources must exist and unsafe propagation is refused.
  Host Git excludes repository-controlled execution settings; integrity checks also work with
  modern Git reference verification without disabling object or reference validation. Fork
  generations and network grants use hardlink anchors, not reusable filesystem metadata. Older
  grants need the documented migration or fresh approval.

- **Parallel forks share one canonical task queue.** Durable assignments and generation-bound
  candidates keep ownership and reviewed completion on the project queue instead of copied queues.
  Claims, leases, release/block transitions and decision history preserve interrupted work.
  Completion rejects unfinished checklists and unsafe no-change claims. Finalization removes only
  contained scratch, retains artifacts and updates state consistently; watch shows task work.

- **Loop completion, review and fallback preserve unfinished work.** Missing or rejected terminal
  actions receive bounded same-session correction. Review debt survives pauses/crashes; protected
  gate sources include declared wrappers. Reviewers consume verification handoffs and request
  missing checks. Authentication, quota and exhausted-credit failures rotate through eligible
  model/provider/account targets with bounded recovery and visible waits.

- **ACP continuity and native capabilities remain intact.** Automatic startup and live selectors
  honor admitted scope. Reopened threads restore recorded native conversations; warm switches reuse
  only compatible boxes. Cancellation and cleanup preserve exact network/session authority.
  Native roles retain declared capabilities, Gemini supports documented thinking levels, and pinned
  Grok tools/login renewal retain their native lifecycle. Usage, delegate and cache accounting
  distinguish evidence from unavailable figures.

- **Independent agent and complete-loop evaluations.** Preview-first `coop eval` includes
  core/queue starters and source-tree shell/Python/Node maintenance examples. Candidate inputs
  and hidden graders are frozen separately; finished work is graded offline without credentials.
  Errors, timeouts and missing trials stay in the requested denominator. Comparisons reject damaged
  evidence and guess no correspondence between multiple configurations. Complete matching runs show
  per-case changes and conservative fixed-suite repeat uncertainty, with explicit assumptions,
  case/repeat counts and an inconclusive result instead of an unsupported winner.

- **Disable ordinary native web tools during evaluations.** Eval candidates cannot use the
  supported clients' normal web-search tools. This is a trusted-client configuration boundary,
  not an adversarial network firewall.

- **CLI, installation and release checks are clearer.** Help, completion, eval discovery, recovery
  guidance and task/network reports describe accepted commands and useful next actions.
  Install and update verify archive checksums; the installer additionally verifies the checksum
  signature when Cosign is installed. Downloads include signed checksums and build provenance.

- **Remote-session structured output is enforced at the completion boundary.** A turn may carry a
  bounded JSON Schema plus the SHA-256 digest of its exact bytes; Coop persists and compiles that
  contract, instructs the model with it, and refuses to complete on malformed or schema-invalid
  output. Schema repairs stay in the same native session and allow three provider responses per
  semantic round. A caller that enables semantic validation receives up to three schema-valid,
  unpublished, digest-bound candidates, then accepts or rejects those exact bytes through an
  idempotent validation endpoint; schema repair does not spend that caller-review budget. Pending
  candidates survive daemon restart, rejection re-prompts the same logical/native turn, acceptance
  publishes only the stored candidate with a durable receipt, and exhaustion publishes no
  assistant message. Once a candidate is staged, provider-runtime teardown is separate retryable
  cleanup: a teardown error cannot hide the candidate, and startup plus the bounded janitor reap
  any leftover process, box, projected credentials, and services without making a validation
  decision or changing the candidate. Accept, reject, and awaiting-turn cancellation likewise reap
  before changing state; a cleanup failure returns a retryable service error while preserving the
  candidate, and the caller retries the still-current decision with a fresh idempotency key after
  runtime recovery.

- **Detached forks have one repository-owned worker-state format.** Current detached workers use
  generation-bound `owner-v2` records; stopped `owner-v1` records remain readable for safe cleanup.
  Coop no longer decodes, signals, rewrites, or partially cleans up headerless pre-v8 records.
  Start/recreate, merge, remove, and stop reject unsupported state before runtime or
  workspace effects while retaining the exact file as lifecycle authority. Stop detached forks
  with v8 before upgrading, or follow the verified process/container procedure in the
  [migration guide](MIGRATING.md#canonical-tasks-across-isolated-forks); never fabricate current
  ownership by prepending the header. Worker, cleanup-pending, reservation, stable-identity,
  exact-owner reap, and atomic-write behavior retain ownership checks. Detached startup hands the
  exact launched reservation from parent to child and publishes the child's PID/token before any child mutation,
  so a successful concurrent stop cannot be undone by a delayed child.

- **Task authority no longer carries an automatic pre-v8 cache migration.** Coop opens only the
  current durable registry at `~/.local/state/coop/task-leases/v1`; it removes the adoption flock,
  cross-volume record copier, staging tree, and rename path from every authority open. If that
  durable root is absent while the retired cache registry contains anything, Coop refuses before
  creating or changing either tree. Stop every older Coop process and migrate the whole directory
  using `MIGRATING.md`; a missing or empty retired root starts clean, while an existing durable root
  is authoritative without consulting the cache. Record formats, inode rechecks, completion trust,
  and current crash recovery are unchanged.

- **Audit reopen authority has one complete-history format.** Coop reads active v3 and
  non-authorizing pending v4 records only. It removes support for the unshipped v1/v2 descendant
  formats and the now-unused `tasks unblock --adopt-audit-head` adoption transaction; unsupported
  versions and unknown fields remain fail-closed instead of being ignored. Current blocked-rewrite
  rebasing, interrupted-unblock recovery, and single-use completion remain unchanged.

- **`coop init` targets the current scaffold only.** Re-init still fills missing current files and
  preserves every existing hook, symlink, and custom hooks path, but it no longer recognizes and
  rewrites pre-v8 hook bytes, root-anchored `.agent` ignore rules, or the retired `.agent/rules/`
  un-ignore. `MIGRATING.md` gives the one-time manual replacements for a direct upgrade from a
  version older than v8.

- **Fork session re-entry uses one exact hint.** Coop now reads only the current
  `.coop/session.<provider>.<account>` record. It no longer adopts provider-only fork records or
  guesses the latest Codex conversation by cwd; a missing or invalid hint starts fresh, and Codex
  records the uniquely new native ID after the run. Exact per-account resume and `--new` remain.

- **Task leases now have one host-only authority.** Coop no longer mirrors its flock and heartbeat
  into provider-visible `tmp/lease.lock` and `tmp/lease.json` files. Acquisition, contention,
  `busy`/`stalled` observation, crash recovery, completion receipts, and audit reopen all use the
  inode-verified registry under `~/.local/state/coop/task-leases/`; task `tmp/` is scratch only.

- **Fleet is removed in favor of direct detached fork loops.** Start each worker with `coop fork
  <name> <target|preset> --loop -d --tasks <path>`; use the canonical shared queue instead of copied
  queue slices, `coop tasks watch` for merged progress, `coop fork ls`/`logs -f` for fork state and output,
  `coop fork stop <name>` to stop one, `coop fork rm <name>` to remove one, and `coop fork merge
  --all` to land all forks. Coop no longer reads `.agent/fleet.yaml`. Removing its manifest parser,
  batch lifecycle, live board, adapter badges, duplicated credential/target resolution, and cast
  leaves fork/session lifecycle ownership and the useful direct primitives unchanged.

- **Fusion mode is removed; its useful pieces are the normal composition model.** Run a reusable
  orchestration recipe directly with `coop <preset>` or `coop acp <preset>`. For an ad-hoc second
  opinion, add repeatable `--peer <target>` flags to an agent, loop, fork, or ACP run. Preset
  native/consult/delegate roles, role personas and ladders, read-only `coop-consult` continuity,
  explicit peer credential scoping, and ACP preset/provider/account switching remain. Removing the
  separate `fusion` grammar, mandatory governor prompt, council state, and duplicate resolution
  paths leaves one lead-and-roles implementation.

- **The security baseline is enforced locally and in CI.** Coop now builds with Go 1.27.1, runs a
  pinned `govulncheck` as part of the one `make check` recipe, and asks Dependabot to watch Go
  modules weekly. MCP configuration also rejects duplicate case-insensitive header names and
  competing inline/environment Authorization sources instead of producing provider-dependent auth.
  Every scoped adapter, including Claude as a direct command or nested peer/preset role, consumes
  one validated immutable snapshot per run, so no consumer can bypass that policy or race a later
  edit to the shared source. Shared and native config reads reject symlinks and special files,
  operate nonblocking on one validated descriptor, and stop at 4 MiB. A malformed, ambiguous,
  unreadable, or oversized file, or a `COOP_MCP_FILE` placed inside a directory Coop mounts
  wholesale, now fails before provider launch. Codex and Grok native TOML is also checked
  semantically: Coop removes only
  byte-preservable canonical `[mcp_servers.*]` tables and refuses dotted, quoted, inline, or other
  alternate declarations before changing any scoped agent home, leaving the configured shared
  `COOP_MCP_FILE` as the sole MCP authority. Remote sessions now cross the same canonical snapshot
  boundary before changing private state and fail the turn before child launch when an active MCP
  authority or adapter projection is invalid, instead of warning and silently answering without
  tools.

- **Remote ACP uses the editor's existing SSH transport instead of a Coop TCP protocol.** Zed
  Remote Development can run `coop acp <target>` beside the remote repository, while a custom
  agent may invoke that same stdio command through `ssh -T`. Coop therefore adds no
  unauthenticated listener, socket bridge, client lifecycle, or second network trust boundary.
  Plain `coop acp` starts automatically with the first signed-in provider; explicit targets and
  presets take precedence. The live Provider and Account selectors remain available after
  connection.

- **An ACP session now starts when the editor spells the repo path differently than git does.** Coop
  mounts the repo at `git rev-parse --show-toplevel`'s spelling, and inside the box — Linux, and
  case-sensitive — that is the only path there is. A macOS editor or shell holding
  `/projects/andrewdryga` for a repo git calls `/projects/AndrewDryga` therefore got
  `cwd does not exist on the machine running the agent` and no session at all. Coop now forwards its
  own spelling when the editor's `cwd` names the same directory. The match is by file identity, so a
  symlinked worktree path works too and two genuinely distinct directories on a case-sensitive host
  are never confused for each other.

- **A container runtime that stopped is now reported as a stopped runtime, not a missing image.**
  `image inspect` fails identically whether the image is absent or the daemon is gone, so a runtime
  that died mid-session told every box command to run `coop build` — a rebuild that could not have
  helped, while the image was present the whole time. Box commands, `coop loop`, and the fork merge
  gate now probe the daemon on that branch and surface the actionable "daemon isn't responding"
  message instead. The probe runs only when the image already looks absent, so a normal launch pays
  nothing for it.

Maintenance scenarios are comparison examples, not official benchmark scores. Repeats do not
establish unseen-task performance.

Releases before 10.1.2 are in [the full changelog at v11.3.0](https://github.com/AndrewDryga/coop/blob/v11.3.0/CHANGELOG.md).
