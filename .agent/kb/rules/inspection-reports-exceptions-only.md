---
name: inspection-reports-exceptions-only
description: "an inspection leads with what happened and prints exceptions only; a qualification prints the checks it proved"
scope: cli-output
sources: [internal/networkreport/report.go, internal/cli/net_cmd.go, internal/box/network_summary.go, internal/box/network_setup.go, internal/cli/doctor.go, internal/cli/net_approved_test.go]
check: "go test ./internal/cli -run 'TestInspectCleanRunIsDestinationFirstAndSilentAboutHealth|TestInspectLifecycleExceptionsAppearOnlyWhenPresent|TestInspectCleanupIsReportedOnlyWhenStillOwed'"
updated: 2026-09-11
---

# An inspection reports what happened and what went wrong — never that things were fine

A command that INSPECTS a record (`coop net inspect`, the run listing, bare `coop net`) leads
with the substance a person came for — the destinations a run reached, what a new run may reach —
and then prints only exceptions: what was blocked, what was flagged, what is missing from the
evidence, what did not run normally, what is still owed. Nothing prints because it is healthy,
invariant, or zero: no mode that every record shares, no policy fingerprint, no `ready ×4`, no
`Alerts none`, no `Cleanup complete`, no `✓` on a normal result. The exit status and the absence
of warnings are the success indication. Anything an exception warning omits stays in `--json`.

A command that QUALIFIES or DIAGNOSES (`coop doctor`, `coop net setup`) is the opposite: its
passed checks ARE the result, one doctor-style ✓/✗ line per property it actually proved, and one
final verdict. The distinction is what the command does, not how much it prints.

**Why:** `coop net inspect` printed twelve labeled facts for a clean run — `Egress filtered`,
gateway epoch, reader timestamps, `Now UNKNOWN`, four healthy layers, `Refused 0 dns, 0 tls`,
`Alerts none`, `Cleanup complete`, `Receipt final, complete` — and omitted the one useful thing,
the destinations the workload reached (task 2026-09-10-make-network-inspection-destination-first).
Every inspectable run is filtered by admission, so the mode is not information; a line on screen
must mean something happened. The same day, `coop net setup` was asked to do the reverse: stop
printing image hashes and timings and show the five security properties its smoke run proved.

**How to apply:**
- Lead with the substance, grouped the way a person names it (host, port, transport; peers
  beneath). Unmeasured stays `UNKNOWN`; a measured zero says so in words (`no external
  connections`), never a row of zeroes.
- Append exceptions only, in the order the operator acts on them, each as `⚠ <headline>` with any
  continuation on its own line indented by exactly two ASCII spaces. No placeholder for an absent
  section.
- Expected states are not exceptions: a live run's open resources, a terminal run's stopped
  gateway, a supervisor still cleaning up. Coop tries its own bounded remedy (recovery) BEFORE
  reporting a problem and reports only the external blocker that remains — never an instruction to
  run the routine thing itself.
- Style the finished plain text: bold heading, yellow `⚠` and headline, dim supporting actions;
  `NO_COLOR` and a non-TTY stream get the same bytes minus ANSI ([[no-color-in-width-fields]]).
- A qualification/diagnostic command prints one line per check that reached a verdict and a final
  verdict; it never claims a later check it did not run.

See also [[command-output-tiers]] (result glyphs belong to standalone results, not to every healthy
fact) and [[tag-exceptions-not-every-row]] (the listing form of the same instinct).

## Changelog
- 2026-09-11 — the CLI design's network packet: the inline box summary is `Networking stats:` with
  per-destination totals (one renderer, `networkreport.View.Inline` — no second formatter), the
  refusal exception is `⚠ Traffic to N remote addresses was blocked` with `coop net blocked` as its
  footer action, and `coop net runs`/`net recover`/bare `coop net` keep their exception-only shape.
  Every state is pinned byte-exact in `internal/cli/testdata/approved/2*.txt`.
- 2026-09-10 — `coop net setup` now prints the checks it proved (`internal/box/network_setup.go`,
  `writeSetupChecks`): one `✓`/`✗` line per property the smoke reached, one bold verdict, no
  runtime/image/timing ledger; a failure claims nothing past the failed check. The pending note
  below is settled. Swept the file: 0 healthy-fact lines remain in the transcript.
- 2026-09-10 — the projection moved to `internal/networkreport` so the interactive box renders the
  same body after its receipt is sealed (`coop: Network run <id>`); the old refusal-only
  `NetworkReport.print` — which printed `nothing was refused` as a healthy fact on every clean box —
  is deleted. The `check:` tests stayed in `internal/cli` beside their fixtures and drive the moved
  code; `TestInlineRunViewIsTheStandaloneBodyUnderCoopsAnchor` proves the two views share one body.
- 2026-09-10 — created from the network-output redesign. Swept `internal/cli/net_cmd.go`,
  `net_result.go`, `net_diagnostic.go`: 0 remaining healthy-fact lines in the run projection, the
  run listing or bare `coop net`; `coop net setup`'s transcript (`internal/box/network_setup.go`,
  `printSetupSummary`) still prints the runtime/image/timing ledger instead of per-check verdicts —
  pending in the same task, owned by the box side.
