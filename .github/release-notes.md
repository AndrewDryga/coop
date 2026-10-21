<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 12.0.1

Coop 12.0.1 fixes certificate verification in Coop and its network helpers by updating to Go 1.27.3 (GO-2026-6809).

After upgrading a session worker, run `coop net setup` as the worker's OS user, using its Docker endpoint and network state directory, before accepting filtered jobs. Create new filtered worker sessions after setup; existing sessions keep the old image records.

For filtered projects with a custom `.agent/Dockerfile`, run `coop build --egress filtered` once; `coop update` already prepares the project where you run it.

Upgrading from Coop 11 or earlier? Read the [Coop 12 upgrade notes](https://github.com/AndrewDryga/coop/releases/tag/v12.0.0) and follow the [migration guide](https://github.com/AndrewDryga/coop/blob/v12.0.1/MIGRATING.md) before updating.

Known issue: intermittent proxy disconnects in filtered Claude sessions remain under investigation.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v12.0.0...v12.0.1)
