---
name: native-helper-file-modes
description: native helper read mounts keep their intended modes under a private umask while outer custody remains owner-only
subsystem: box
sources: [internal/box/native_broker.go, internal/box/native_open.go, internal/box/native_broker_test.go]
updated: 2026-10-11
---

`nativeRun.prepare` owns a mode-0700 outer directory. Its `guard` child is a
mode-0755 read mount for a separate helper UID; static config, public CA and
expiring access snapshots have mode 0444. The workload never mounts guard
custody or receives its real grant. `startNativeOpen` runs the helper as
65532:65532 with its existing capless, read-only boundary.

Requested creation modes are reduced by the operator's umask. A managed
worker using 077 previously created guard as 0700 and CA/config as 0400,
so the helper exited before readiness and the model never started. The
new guard is explicitly chmodded; static files use the existing atomic
mode writer. The outer directory remains 0700. The private-mask regression
uses a subprocess and the real inert credential-safe publication fixture.

## Changelog
- 2026-10-11 — recorded the observed helper-UID denial and restrictive-mask
  regression; verified outer custody, read mounts and public-only projection.
