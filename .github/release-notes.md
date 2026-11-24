<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 15.4.0

- **Claude Code can set up its official marketplace in filtered boxes.** A fresh interactive session can register the marketplace through two bounded download requests. Other destinations still follow project network policy.
- **Filtered boxes ride out brief gateway stalls.** Coop gives the proxy time to recover under the same network rules. If the gateway stays unavailable, Coop stops the box and records the reason.
- **Interactive boxes make it easier to pick up where you left off.** All four clients show an exact Coop resume command when a saved conversation changes, and supported terminals retain true color.
- **Bundled clients and runtime components are refreshed.** Coop updates its shared image on the next launch. If a custom project image is stale, run `coop build`. Filtered runs redo network setup once.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v15.3.1...v15.4.0)
