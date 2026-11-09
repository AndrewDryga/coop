<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 13.2.0

Coop 13.2.0 moves every agent in the box to its latest release.

- Claude Code 2.1.319, Codex 0.166.0, Grok 1.0.77, Claude's editor adapter 0.101.0, Codex's editor adapter 2.6.0 and Playwright 1.65.0. Gemini 0.67.0 was already current.
- Filtered sign-ins keep working with the requests the new clients send, and a rejected Claude key is still reported as a sign-in problem.

Upgrading:

- The next launch rebuilds the box image, and filtered runs redo their network setup once.
- In editor sessions, Claude accepts only the models its adapter lists: the short names (`opus`, `sonnet`, `haiku`, `fable`) and the current full IDs. Use `haiku` instead of an older ID such as `claude-haiku-4-5`.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v13.1.0...v13.2.0)
