<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 11.3.0

Usage estimates are more complete, repeat checks load faster, and workers stay responsive while repositories download.

- Large CLI histories no longer crowd recent requests out of the 30-day estimate. Unchanged histories reuse cached token records, without storing conversation text.
- Repository and submodule downloads run in the background, and workers clean up copies that sessions no longer need.
- Session setup accepts the requested commit after its branch advances. Commits removed by a rewritten branch are still refused.

Known issue: intermittent proxy disconnects in filtered Claude sessions remain under investigation.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v11.2.0...v11.3.0)
