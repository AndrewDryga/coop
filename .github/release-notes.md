<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 15.0.0

Coop 15.0.0 closes three ways a fork merge could land something its review never saw, lets a reviewed fork that conflicts with your branch be merged, and carries the fixes from a manual test of every major feature on real Docker and real agents.

- Upgrade warning for session workers: the session store moves to schema 27 on first start and an older build then refuses it, so a worker cannot be rolled back. Copy the stopped state folder first; MIGRATING.md has the steps.
- Fork merges land only what was reviewed: a fork with a merge commit or a shallow repository is refused, and when your branch already has one of the fork's commits Coop checks that the rebase lands exactly the reviewed change.
- A reviewed fork that conflicts with your branch is no longer a dead end: `coop fork <name> --loop` has its agent rebase and resolve, the result is reviewed again, and `coop fork merge` lands it.
- Services: a box's command starts only after its service ports accept connections (about one start in three used to be refused), `coop down` works when the Compose file is broken or gone, and a failed service is named with its output. `COOP_SERVICE_<NAME>_URL` now names a service's first port; a `coop.service.port` label picks another.
- Session service: a publish interrupted by a worker stop resumes after restart, cancelling a turn ends it cancelled, a clean stop leaves a running turn interrupted, and several errors now name the real problem. Controllers that match on error answers should read the changelog.
- Filtered Codex runs no longer end with a blocked-traffic warning for `oaiusercontent.com`, and `coop codex -- plugin list` works in a box.
- `coop usage` counts history in a provider home that is a link, and many messages across the command line now say what went wrong and what to run next.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v14.0.0...v15.0.0)
