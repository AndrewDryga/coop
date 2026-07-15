---
name: provider-scripted-e2e
description: Drive the external Coop CLI through strict runtime/provider fixtures without ambient state
subsystem: testing
sources: [Makefile, internal/testutil/procharness/harness.go, internal/cli/scripted_process_e2e_test.go, internal/cli/direct_process_e2e_test.go, internal/cli/testdata/providerfixture/main.go]
updated: 2026-07-15
---

`make provider-scripted-e2e` builds fresh Coop and fixture executables inside a disposable root,
then drives a direct process matrix for every registered provider. The child environment is built
from an empty allowlist, all mutable/config paths live below that root, and the harness owns the
outer process group and bounded output (`internal/testutil/procharness/harness.go:74`,
`internal/testutil/procharness/harness.go:231`). The matrix proves marked-default and explicit
account selection, configured and inline model/effort precedence, exact adapter argv/env, forwarded
arguments, credential stripping, output passthrough, fail-fast validation, nonzero exit propagation,
and ready-synchronized cancellation (`internal/cli/direct_process_e2e_test.go`).

The fixture is deliberately not a container emulator. It accepts only Coop's tested runtime verbs
and flags, validates every host bind/env-file/workdir before invoking its own explicit provider mode,
and writes a versioned 0600 JSONL trace. Unknown syntax, images, commands, symlinks, and root escapes
fail closed (`internal/cli/testdata/providerfixture/main.go:199`,
`internal/cli/testdata/providerfixture/main.go:394`,
`internal/cli/testdata/providerfixture/main.go:1028`). Real runtime isolation belongs to `coop
doctor`: CI blocks on Docker and Podman (`.github/workflows/ci.yml:53`) and separately exercises the
review-write boundary (`.github/workflows/ci.yml:84`); Apple's runtime still requires a local doctor
run because the hosted matrix cannot cover it.

Scenarios are a closed data contract: bounded plain output, exit status, or wait-for-signal. They
cannot contain commands, shell fragments, unknown fields, or control characters. To extend it, add
scenario data and semantic assertions to the process suite; never branch on a Go test name or
execute the provider tail received from runtime argv. A failure diagnostic includes the
bounded stdout/stderr and trace. Trace host paths are fixture-relative, environment values are
default-redacted, and free-form label/provider values use deterministic digests; the whole trace is
deleted with the test root (`internal/cli/testdata/providerfixture/main.go:759`,
`internal/cli/testdata/providerfixture/main.go:787`,
`internal/cli/scripted_process_e2e_test.go:89`).

## Changelog
- 2026-07-15 - expanded to direct target/account/model/effort, failure, exit, and cancel behavior
- 2026-07-15 - created with the strict all-provider direct-process harness
