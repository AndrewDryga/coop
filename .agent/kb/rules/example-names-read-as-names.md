---
name: example-names-read-as-names
description: an example fork name in help is never a Coop command, fork verb or provider name
scope: cli-output
sources: [internal/cli/fork_cmd.go, internal/cli/help.go, internal/forkctl/ls.go, internal/cli/workflow_help_approved_test.go]
check: go test ./internal/cli -run TestForkHelpExampleNamesAreNotCoopWords
updated: 2026-10-04
---

# Pick example names that cannot be read as Coop words

An example value in help sits in a slot whose grammar the reader is still learning. When a fork
name in an example is also a Coop command, fork verb or provider name, the example reads as syntax
instead of a name. Use a name that is plainly the reader's own: the fork pages use `myfork`.

**Why:** the fork pages showed `coop fork login claude`, `coop fork review login` and
`coop fork rm login`. The user (2026-10-04): "login is a bad example, we need some other fork name
more obvious otherwise it looks like you need to login forks".

**How to apply:**
- Check a new example name against `topLevelCommands`, `forkspace.VerbList()` and the provider
  registry before using it.
- A task id or title that only contains a command word (`login-retries`, "Fix login retries")
  reads as a task, so it stays.
- The check covers the fork family page, every fork command page and the ACP page's fork example,
  which the generated manual copies. Runtime hints such as the empty `coop fork ls` listing are
  reviewed by hand.

## Changelog
- 2026-10-04 — created from the user's correction. Swept the fork family page, the eight fork
  command pages, the ACP page, the empty `coop fork ls` hint and code comments: 20 `login` fork
  examples became `myfork`, and the generated docs followed. Swept task, backlog and preset
  examples for the same collision: none (`login-retries` stays). The new test failed on the old
  family page and passes on the renamed one.
