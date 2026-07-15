---
name: acp-scripted-e2e
description: ACP state machines are exhaustive in the scripted runtime; real adapters get an isolated conformance layer
subsystem: acp
sources: [Makefile, internal/acpproxy/scripted_matrix_e2e_test.go, internal/acpproxy/e2e_test.go, internal/acpproxy/testdata/acpfixture/main.go]
updated: 2026-07-15
---

`COOP_RUNTIME` lets process tests drive the built outer supervisor, inner re-exec, controller,
proxy, target resolution, and stdio while a semantic JSON fixture owns provider replies. The
scripted matrix covers every directed provider pair and deterministic model, limit, replay, and
failure orderings (`internal/acpproxy/scripted_matrix_e2e_test.go:41`,
`internal/acpproxy/scripted_matrix_e2e_test.go:111`). Run it with `make acp-scripted-e2e`; it does
not require provider credentials (`Makefile:54`).

`make acp-e2e` is the smaller installed-adapter contract. It uses credential-only profile overlays,
an isolated config/XDG environment, and a disposable writable repo, then proves live
prompt/model/SIGHUP behavior and cross-provider carry (`internal/acpproxy/e2e_test.go:37`,
`internal/acpproxy/e2e_test.go:126`, `internal/acpproxy/e2e_test.go:756`,
`internal/acpproxy/e2e_test.go:876`). Never induce real quota exhaustion there; provider fault
injection belongs in the scripted layer.

Related: [[acp-replay-publication]], [[acp-target-commit]], [[acp-carry-echo]].

## Changelog
- 2026-07-15 - split test-boundary fact from replay, target, and carry contracts; verified matrix and live drivers
- 2026-07-14 - created after replacing manual Zed reproduction with the scripted runtime driver
