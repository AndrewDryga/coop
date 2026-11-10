<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 13.2.1

Coop 13.2.1 patches box tooling and keeps large diagnostic attachments from overwhelming model prompts.

- Box images use patched system packages and npm. asdf is rebuilt with the patched Go runtime, preserving its existing version and toolchain behavior.
- Large text attachments remain available as files; models receive a short reference and can read the full contents when needed.
- Daemon cleanup keeps completed task results, avoiding unnecessary retries when daemon children are already exiting.

The next launch rebuilds the shared box image. Rebuild project-specific images with `coop build`; filtered editor sessions may ask you to run `coop net setup` once.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v13.2.0...v13.2.1)
