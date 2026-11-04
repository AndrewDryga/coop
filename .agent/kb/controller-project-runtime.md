---
name: controller-project-runtime
description: frozen controller selectors provide project toolchains and workspace-owned test services
subsystem: worker
sources: [internal/workerproto/job.go, internal/box/controller_runtime.go, internal/box/run.go, internal/box/services.go, internal/sessionsvc/job.go, internal/sessionsvc/service.go, docs/session-api.md]
updated: 2026-11-04
---

# Project tools and services in controller jobs

A controller job can carry a `runtime` object alongside its environment and check:

```json
{"version":1,"dockerfile":".agent/Dockerfile","compose":"dev/services.yml","environment":{"PGHOST":"db"}}
```

The controller resolves these repository-relative paths from the exact source it grants. An empty
Dockerfile selects the worker's shared image; an empty Compose path selects no services. The runtime's
`environment` contains frozen project defaults, bounded to 64 KiB of escaped JSON. Candidate review
settings override these defaults; the job's controller environment and check values stay strongest.
The worker never imports project egress,
resource limits, credentials or MCP settings. The optional runtime is covered by the job digest;
retained jobs without it keep their original authority and digest. A controller must place a job
carrying runtime only on workers advertising `project-runtime:1`, proven by the local daemon's
`project_runtime_versions: [1]` response. Job setup remains version 2.

Project toolchain images build from the private frozen source using Coop's sanitized build context.
The cache identity includes that source, Dockerfile and worker base image. Work and its review use
the same granted toolchain. Project builds require open networking; filtered controller toolchains
are refused explicitly. Services require an open, Compose-capable runtime.

Each Work workspace owns its Compose project and default network. At least one service must join
the default network; custom-only network topologies are refused before startup. Service aliases
and additional project networks are preserved. The host validates and snapshots
the Compose file before starting it, waits for health checks, and connects the box by service name.
A required file, service or network that is unavailable prevents the command from running. Service
containers inherit the job's finite CPU, memory and process caps. Host port publication is removed;
services communicate over the private network. External and globally named volumes are refused.
The normal host-path, privilege, secret-shadowing and literal-environment checks still apply.

Named service volumes survive ordinary turns on the same worker. Containers, anonymous volumes
and unused networks are removed when the runtime stops; tmpfs and anonymous data are disposable.
Final discard removes named volumes before deleting the workspace. Cleanup uses the immutable
workspace project label, so deleting or breaking the Compose or project file cannot strand the
stack. Failure remains retryable and prevents workspace deletion. Checkpoints transfer code and
job state, not database volumes: a replacement worker reconstructs disposable test services.

A review uses its granted toolchain, project defaults and Work stack by default. A candidate's `review.compose`
and `review.env` can provide a separate test stack, as before. A declared review stack is required,
and its containers, network and named volumes are removed when the review ends.

## Changelog
- 2026-11-04 — added the frozen project-runtime contract, tested toolchain/service startup,
  per-job PostgreSQL isolation, named-volume continuity, and cleanup after Compose deletion.
