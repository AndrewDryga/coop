<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 15.3.1

A fix for a 15.3.0 regression, and a safety fix for killed loops.

- **A killed loop's box no longer keeps working.** If a loop or `coop run` was killed outright (`kill -9`, a crash), its agent kept running with nobody watching and could still commit to your repository. Now a small helper started with each launch removes the box within seconds. It never touches a box whose owner is still running.
- **`coop help tasks ls --all` works again.** 15.3.0 refused a command's own options after `coop help`.
- **A launch that needs `coop build` says so**, with the build command on its own line, and the loop's status shows "project box needs a build" instead of "loop failed".

[Full changelog](https://github.com/AndrewDryga/coop/compare/v15.3.0...v15.3.1)
