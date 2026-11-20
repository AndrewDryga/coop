<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 15.2.1

Three small fixes found while testing 15.2.0 by hand.

- Restoring a checkpoint brings back the workspace's empty task queue folders. Restore used to drop them, and `coop tasks lint` then flagged the queue.
- The refusal for a fork commit that your branch already has now fits a terminal; its lines used to run past 200 columns.
- A checkpoint refused for a credential still names no file path. This is on purpose and now documented: an agent chooses file names, so a path could carry the value.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v15.2.0...v15.2.1)
