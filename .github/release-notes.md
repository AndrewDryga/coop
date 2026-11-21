<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 15.3.0

Coop now runs Claude Fable 5.2 and GPT-6.2, adds a fast mode, and carries the fixes from a full manual test of the main features.

- **New models and clients.** Claude Code 2.1.330 with Fable 5.2 and Haiku 5.5, Codex 0.170.0 with GPT-6.2 Sol and Astra and GPT-6.1 Terra. The next launch rebuilds the box image.
- **Fast mode.** End a Codex or Claude target with `+fast`, as in `coop codex:gpt-6.2-sol+fast`, to run it in the provider's faster, pricier mode.
- **Closing tasks gets out of your way.** A task with follow-up commits closes without `--force`, a forced close is no longer stopped by another task's edited archive, and a loop review left unfinished weeks ago no longer blocks closes or makes every later loop fail.
- **Security: links inside `.git` are refused.** An agent in a box could make `.git/commondir` or `.git/config` a link to a file outside the repository, and Coop would quote that file in an error the box could read, or take remote URLs from it. 15.2.1 and earlier are affected.
- **Typos no longer run quietly.** A Claude effort that Claude Code does not have, such as `claude:opus/hgih`, is refused before the box starts instead of running at the default effort.
- **Session service.** A store call cut short by a timeout no longer leaves later writes failing with "database is locked". A worker reports each account's remaining allowance to its controller.
- **Clearer refusals** across `coop help`, tasks, forks, presets and networking, each with the next step. A shallow clone is refused with the reason and `git fetch --unshallow`.

Upgrading: a controller that decodes the worker hello strictly must accept `account_usage` before its workers upgrade. See [MIGRATING.md](https://github.com/AndrewDryga/coop/blob/v15.3.0/MIGRATING.md) for this and the stricter checks.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v15.2.1...v15.3.0)
