---
name: provider-scripted-e2e
description: Drive the external Coop CLI through strict runtime/provider fixtures without ambient state
subsystem: testing
sources: [Makefile, internal/testutil/procharness/harness.go, internal/cli/scripted_process_e2e_test.go, internal/cli/testdata/providerfixture/main.go]
updated: 2026-07-15
---

`make provider-scripted-e2e` builds fresh Coop and fixture executables inside a disposable root,
then drives one direct process smoke for every registered provider. The child environment is built
from an empty allowlist, all mutable/config paths live below that root, and the harness owns the
outer process group and bounded output (`internal/testutil/procharness/harness.go:72`,
`internal/testutil/procharness/harness.go:219`).

The fixture is deliberately not a container emulator. It accepts only Coop's tested runtime verbs
and flags, validates every host bind/env-file/workdir before invoking its own explicit provider mode,
and writes a versioned 0600 JSONL trace. Unknown syntax, images, commands, symlinks, and root escapes
fail closed (`internal/cli/testdata/providerfixture/main.go:195`,
`internal/cli/testdata/providerfixture/main.go:390`,
`internal/cli/testdata/providerfixture/main.go:965`). Real runtime isolation belongs to `coop
doctor`: CI blocks on Docker and Podman (`.github/workflows/ci.yml:53`) and separately exercises the
review-write boundary (`.github/workflows/ci.yml:84`); Apple's runtime still requires a local doctor
run because the hosted matrix cannot cover it.

To extend it, add scenario data and semantic assertions to the process suite; never branch on a Go
test name or execute the provider tail received from runtime argv. A failure diagnostic includes the
bounded stdout/stderr and trace. Trace host paths are fixture-relative, environment values are
default-redacted, and free-form label/provider values use deterministic digests; the whole trace is
deleted with the test root (`internal/cli/testdata/providerfixture/main.go:696`,
`internal/cli/testdata/providerfixture/main.go:724`,
`internal/cli/scripted_process_e2e_test.go:89`).

## Changelog
- 2026-07-15 - created with the strict all-provider direct-process harness
