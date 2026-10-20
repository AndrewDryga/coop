<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 12.0.0

Coop 12 strengthens project isolation, improves filtered networking, and makes session startup and cleanup more reliable.

- Fork and task records are better protected from boxes that can write nearby projects. Mac boxes and project services hide secret files under every letter-case spelling, and filtered boxes cannot bypass network rules through Docker's name server.
- Running editor conversations pick up newly approved network rules between prompts, without reconnecting. Each running box keeps the rules it started with.
- Large companion repositories prepare faster and in parallel. Agent starts skip unchanged legacy history, and remote turns survive brief database contention before the agent starts.
- Runs recover from brief sign-in renewal failures. Cleanup removes more containers, services and images left by interrupted runs. Signing no longer rewrites pushed commits.

### Upgrading

- **Stop fork users first.** Stop fork loops, fork editor sessions and session services before upgrading. Fork workspaces and your home folder must share a disk. Then run `coop fork ls` in each project with forks before launching a box from a folder containing several projects. Read [the migration guide](https://github.com/AndrewDryga/coop/blob/v12.0.0/MIGRATING.md) before downgrading.
- **Prepare filtered networking again.** Run `coop net setup`, then `coop approve` in each filtered project. Recreate existing worker sessions. Projects with a custom `.agent/Dockerfile` also need `coop build --egress filtered` once.
- **Replace removed modes.** `--readonly` and `--bare` are gone. Use `--peer` for a second opinion and a fork for isolation. Worker jobs use `mode: "normal"`, with `repository_read_only: true` when needed. Existing sessions in removed modes remain readable but cannot resume.
- **Check workers before upgrading.** Stop and back up the worker's session data, then use the new binary's `coop sessions check --state <path>` before starting it against that data.
- **Review Mac secret filenames.** A hidden filename with more than 11 letters can now stop a launch on a case-insensitive disk. Move it into a hidden folder or shorten its name; see [hiding secrets](https://github.com/AndrewDryga/coop/blob/v12.0.0/docs/box.md#hiding-secrets).
- **Check image tools and automation.** Images no longer bundle Staticcheck, govulncheck or `jv`; install them in your project's `.agent/Dockerfile` if needed. Consumers of `coop tasks watch --json` must stop expecting `starting`.

Known issue: intermittent proxy disconnects in filtered Claude sessions remain under investigation.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v11.3.0...v12.0.0)
