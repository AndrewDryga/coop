---
name: investigate
description: Root-cause a crash, panic, failing test, or wrong behavior — reproduce, read the real error, trace to the actual cause, then propose the minimal fix. Use when something fails or misbehaves and you need the cause, not a guess. Never edit a test to make a real failure pass.
argument-hint: "<the error, failing test, or symptom>"
allowed-tools: Read, Grep, Glob, Bash
---

# Investigate — find the cause, not a symptom

Discipline beats guessing. Do **not** propose a fix until you can point at the line
that's wrong and say *why*. Never edit a test to make a real failure pass.

## Method

1. **Reproduce.** Run the exact failing command (`go test ./pkg -run TestX`, the CLI
   invocation, the request). No repro → you're guessing.
2. **Read the WHOLE error.** The full panic / stack trace / output, not the summary
   line. The first frame in *our* code (not a dependency) is usually the spot.
3. **Locate + read the code.** Open the failing function and the data it touched.
   Confirm the contract of anything you're unsure of (`/verify-api`) — half of "bugs"
   are an assumed return shape, arg, or flag that was never real.
4. **One hypothesis, then confirm it** against the code/data *before* fixing. If the
   evidence doesn't match, the hypothesis is wrong — don't fix anyway.
5. **Minimal fix at the cause.** Then add a **regression test** that fails before and
   passes after (plus the failure / denial path where it applies).

## Common causes — check these before theorizing

- **An assumed API** — a return shape, error value, flag, or option that doesn't exist
  as you think. `/verify-api` it against the real source.
- **Environment / path** — a value from env or cwd that's empty or different at run
  time than in your head (absolute vs relative; `$TMPDIR` under `/var` resolving to
  `/private/var`).
- **A test race** — shared state or `t.Parallel()` without isolation, or an async
  effect not made deterministic. Never `sleep` it away; synchronize on the signal.
- **Stale build / cache** — you're running old code or a cached result. Rebuild clean
  and reproduce before concluding.

## Output

`Cause: <file:line — what's wrong and why>` → `Fix: <the minimal change>` →
`Regression test: <what it asserts>`. If you can't isolate it, say what you ruled out
and what you'd instrument next — don't ship a guess.
