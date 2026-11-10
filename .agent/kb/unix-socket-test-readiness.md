---
name: unix-socket-test-readiness
description: Observation fixtures wait for an accepting Unix listener before their one-shot protocol assertions
subsystem: development
sources: [internal/networkgateway/clock_proof_test.go, internal/networkgateway/observe_test.go, internal/networkgateway/observe.go, internal/testutil/wait/wait.go]
updated: 2026-11-10
---

A Unix socket pathname appears at bind, before listen accepts connections. The observation
fixtures use `wait.For` around a successful bounded dial, then run their protocol assertions once.
File existence alone raced startup in `TestObservationPeerEquivalenceDoesNotRebindHostCustody`;
`TestPrivateObservationReadsCacheWithoutProbingOrRenewing` already used the connection boundary.
The peer-equivalence and exact host-custody checks stay outside the readiness poll.

## Changelog
- 2026-11-10 — matched both observation fixtures after the broad gate caught the remaining
  pathname-only waiter; deterministic bound-without-listen reproduction confirms the window.
