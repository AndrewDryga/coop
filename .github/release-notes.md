<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 13.0.0

Coop 13.0.0 makes filtered networking the default, brings full secret protection to Linux, and makes forks, loops and editor sessions far more dependable.

- Boxes now use filtered networking by default, and the no-network mode is called `offline` everywhere.
- On Linux, ext4 and xfs projects get the same kernel protection as Mac projects: a secret you save after a box starts stays hidden, and `coop doctor` shows kernel protection on Ubuntu 24.04.
- Forks keep their model, account, approvals and service files; fork loops keep working when one task needs your decision, and `coop tasks watch` shows what is moving, what stopped and what needs you.
- Tasks can have a priority, wait on another task, or be set aside without a question, and loop workers run only their task's checks instead of the whole gate every time.
- Editor sessions stay connected, start when the project box is not ready yet, and two sessions in one project no longer wait on each other.
- `-p "…"` runs one prompt on every agent, and help, refusals and network reports use plainer words.

Breaking changes:

- Coop 13 no longer converts records written by Coop v12.1.0 or earlier (old forks, tasks, network approvals, host state and session data), nor logins and history from before v11. It refuses them by name and changes nothing. Most installations have none; if Coop names one, open that project once with Coop v12.1.0 first. The [migration guide](https://github.com/AndrewDryga/coop/blob/v13.0.0/MIGRATING.md) has the steps.
- `coop net approve` is gone; use `coop approve`.
- Scripts reading `coop tasks watch --json` see task states named like its counts.
- `~/.config/coop` now holds only settings and accounts; per-machine records moved out.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v12.1.0...v13.0.0)
