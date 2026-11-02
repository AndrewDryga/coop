<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 12.1.0

Coop 12.1.0 improves Mac startup and secret protection, especially in large projects.

- Supported APFS Docker runtimes protect secret files without creating thousands of case-variant mounts. No extra host packages are needed, and each run receives fresh access rules.
- Other Mac runtimes can remember reviewed case-exposure exceptions through `coop approve`, instead of requiring the environment override on every launch.
- Closed sessions can retain workspace checkpoints, including task notes and uncommitted changes, without restarting the agent.
- Filtered Codex sessions now allow the client's image-generation and editing requests.
- New walkthroughs for Claude Code, Codex, Gemini CLI and Grok CLI take you from setup to checking secret, credential and network protection.

After upgrading a session worker, run `coop net setup` as the worker's OS user, using its Docker endpoint and network state directory, before accepting filtered jobs. Create new filtered worker sessions after setup.

If Coop requests a project image rebuild, run `coop build --egress filtered` in that project.

Restart affected agents or services after changing `.coopignore`, newly classifying existing files as secret, or creating new host aliases to secrets. Running protection does not reload those changes.

Upgrading from Coop 11 or earlier? Follow the [migration guide](https://github.com/AndrewDryga/coop/blob/v12.1.0/MIGRATING.md).

Known issue: intermittent proxy disconnects in filtered Claude sessions remain under investigation.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v12.0.1...v12.1.0)
