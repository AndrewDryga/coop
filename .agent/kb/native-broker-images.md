---
name: native-broker-images
description: Codex image generation and editing use separate exact provider routes from text inference
subsystem: box
sources: [internal/agent/codex.go, internal/agent/codex_image_test.go, internal/networkgateway/native_image_test.go, internal/networkgateway/main_test.go, Makefile]
updated: 2026-10-29
---

The pinned Codex 0.159.2 image tool posts JSON to `images/generations` or `images/edits`
through its selected model provider. ChatGPT uses `/backend-api/codex` and API-key accounts
use `/v1`. Each full path is an exact POST route with no query. Both families retain their
existing Authorization mediation; only ChatGPT carries the selected account header.

The upstream definitions are `codex-rs/codex-api/src/endpoint/images.rs` and
`codex-rs/model-provider-info/src/lib.rs` at `openai/codex` tag `rust-v0.159.2`.
Text inference succeeding does not prove image routing: a production image request reached
the tool but failed immediately with broker403 because these routes were absent. No image
was created, so artifact transport was not the failing boundary.

The transport regression consumes the adapter's routes and exercises CONNECT/HTTP2 with
selected credential replacement, exact request bytes, upstream errors, and denied adjacent
request shapes. The adapter regression separately pins the real origin and authentication. Its test-only adapter
import requires the gateway TestMain to isolate state and the agent check scope to include
the gateway and its reverse consumer.
Worker binaries must be rebuilt to publish updated declarations; an existing running worker
does not read adapter source changes from a controller deployment.

## Changelog
- 2026-10-29 — reproduced the missing image routes before repair and added both operations
  for both credential families without widening matching or credential access.
