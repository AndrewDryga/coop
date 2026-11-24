<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 15.4.1

- **Claude editor sessions can start when managed policy blocks the supplied MCP servers.** Claude skips those servers and reports which ones it skipped.
- **Updated the bundled editor adapters.** Claude ACP moves to 0.111.0, and Codex ACP 2.11.0 brings editor protocol and terminal-output fixes.

If Coop reports a stale custom project image, run `coop build`.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v15.4.0...v15.4.1)
