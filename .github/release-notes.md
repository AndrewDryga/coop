<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 15.1.0

Controllers can now assign different named presets to code reviews and coding jobs. Export a global preset with `coop presets NAME --export-job`. Each job keeps the prompts and accounts it was assigned, and review history shows which peers answered.

- Updating an existing pull request now waits for GitHub to show the pushed commit before reporting publication. A missing source commit or branch produces a refusal after three tries.
- A manually rebased fork runs its checks before review. When a task commit is already on the target branch or a merge commit needs a manual rebase, Coop explains how to proceed. Finish any such merge requests before downgrading to 15.0.0.
- Workers upgrading from 14.x or earlier should back up their stopped session store first. The 15.x store upgrade cannot be rolled back. See [the migration guide](https://coop.dryga.com/migrating.html).

[Full changelog](https://github.com/AndrewDryga/coop/compare/v15.0.0...v15.1.0)
