<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 15.2.0

This release fixes a security problem in isolated forks. When your branch already had one of an isolated fork's commits, `coop fork merge` could land a line that no review had seen. The merge now compares what would land with what was reviewed and refuses a difference. Versions up to 15.1.0 are affected; ordinary forks got the same check in 15.0.0.

- A new isolated fork no longer has a Git remote pointing at your checkout. `coop fork ls` shows an isolated fork whose merge was refused, and fork messages name the next command to run.
- `coop fork rm --gone` removes the records Coop kept for repositories you deleted. It lists them and asks first.
- A first Ctrl-C in a fork loop finishes the running pass and stops before the next one, with exit code 130.
- A worker can checkpoint work that was rebased onto the job's newer default branch, and another worker can restore it. Both workers need this version, and they advertise `checkpoint-upstream:1`.
- Publishing has two new refusals for controllers to handle: a branch that carries someone else's closed pull request, and a first publish whose push may have landed. A controller should treat any refusal code it does not know as final. See [the migration guide](https://coop.dryga.com/migrating.html).
- A worker waits between source grant requests while the controller is unavailable, and it prunes its publication journal after 30 days.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v15.1.0...v15.2.0)
