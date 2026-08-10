---
name: loop-live-bar
description: the loop's sticky bottom bar; every paint parks the cursor at column 0 so a kernel ^C echo can't wrap the line and desync the region's erase math — the subsystem once deleted instead of debugged
subsystem: loop
sources: [internal/loop/bar.go, internal/loop/iteration.go, internal/loop/loop.go, internal/ui/live.go, internal/ui/ui.go, internal/loop/bar_test.go, internal/loop/interrupt_test.go, internal/ui/live_test.go, internal/ui/region_skip_test.go]
updated: 2026-08-10
---

`coop loop` renders a Docker-build-style live view: the agent's output scrolls in the region
above a sticky one-line bar (spinner, state bar, counts, activity, elapsed) pinned to the bottom
of the MAIN screen — not the alternate buffer, so the whole run stays in scrollback afterwards.
That choice is the reason everything below is cursor arithmetic.

**What paints, and when.** `runIteration` builds the live stack only when BOTH stdout and stderr
are terminals (`internal/loop/iteration.go:123`, `internal/loop/bar.go:18-20`) — the agent's stdout
is funneled into stderr's region, so a redirected stdout would lose its bytes into the bar.
Terminal IDENTITY is deliberately irrelevant: Warp gets the bar like everything else
(`bar.go:15-17`; an earlier Warp exclusion was reverted). The region writes to `os.Stderr` and
re-samples `ui.TermWidth(os.Stderr)` on every paint, so a resize is picked up
(`iteration.go:130-135`). Three writers cause a repaint, and all of them funnel through
`loopBar.render` → `Region.Update(history, []string{line})` (`bar.go:57-62`):

1. one whole line of agent/loop output, buffered by `lineWriter` (`bar.go:108-127`) — partial
   lines never paint, so a half-written line can't be measured as a full one;
2. a queue-count change, polled every `progressPoll` = 2s (`iteration.go:29,298-321`) — counts
   only, never the activity string, and a torn read of 0/0 is rejected (`iteration.go:316-318`);
3. the spinner tick, every `spinInterval` = 120ms (`bar.go:13,83-94`), started only when
   `ui.SpinnerEnabled()` (`iteration.go:199-202`).

Coop's own status lines join the same funnel: `ui.SetLiveSink(bar.history)` routes every
`ui.Info`/`ui.OK` — including `box.Run`'s startup notices — into the history above the bar instead
of overprinting it (`iteration.go:139-140`, `internal/ui/ui.go:104-132`). Teardown order is load
bearing: stop the goroutines and `wg.Wait()` first so nothing repaints afterwards
(`iteration.go:245-248`), then `funnel.flush()`, then `bar.stop()` → `Region.Clear`
(`iteration.go:275-278`, `bar.go:80`, `internal/ui/live.go:115-120`).

**The desync this subsystem exists to avoid.** An interactive terminal echoes Ctrl-C as a literal
`^C` at the cursor. Before the fix, a paint left the cursor after the bar's last cell: the echo
filled the final column, triggered the terminal's pending wrap, and silently dropped the cursor one
line down. From then on every `eraseLocked` (`live.go:124-133`) ran a line too low, so each Ctrl-C
leaked a stale bar frame into scrollback with coop's stop notice glued to it.

**The invariant: park the cursor at column 0 after every paint** (`live.go:102-108` — a bare `\r`
once the region has lines). The echo then overtypes the bar's own first cells and the next repaint
wipes it, instead of extending the line past the final column. Pinned by `live_test.go:201-206`.
Three companions keep the same arithmetic honest, and breaking any one of them reopens the bug:

- **region lines are clipped to `width-1`** so a bar line can never wrap on its own
  (`live.go:99`, budgeted in `bar.go:47-49`);
- **nothing writes a raw newline to stderr while the bar is up.** `loopInterruptInfo` keeps its
  fresh-line guard only on the plain line-oriented path and goes through `ui` alone when
  `ui.LiveActive()` (`loop.go:45-56`, `ui.go:110-117`, pinned by `interrupt_test.go:96-108`);
- **`Clear` ENDS the region** (`live.go:119`): a status line racing teardown appends as a plain
  line rather than repainting an ownerless ghost bar (`region_skip_test.go:33-46`).

The "wiped within 120ms" promise in `live.go:104-107` is the spinner's cadence, so it holds only
while the spinner runs; with `COOP_SPINNER=0` no ticker exists and an identical frame is skipped
(`live.go:86-88`), so the wipe waits for the next real change. In the Ctrl-C case that change is
immediate anyway — the stop notice arrives as history, and non-empty history always repaints
(`live.go:86`, `loop.go:289,292,302`).

**Verifying a TUI fix with nobody watching (VT replay).** The technique 17cee39 used, and the one
to reuse for any change to the erase math: drive the REAL bar/region/funnel stack through the
reported session in a harness, injecting the tty's echo bytes between paints; dump the resulting
byte stream; replay it through a small VT model implementing autowrap/pending-wrap, CR, LF, and
CSI K/J/A. The proof is two-sided — with the fix stripped, the stream must reproduce the reported
corruption character for character; with it, the screen renders clean. That harness was never
committed: today's standing pins are byte-level assertions only (`live_test.go:189-228`,
`region_skip_test.go`, `interrupt_test.go`), so rebuild the model if you touch cursor motion.

**Where the state lives.** The bar owns no cursor state — only its own fields under `b.mu`
(`bar.go:26-34`). ALL cursor bookkeeping is `ui.Region`'s: `shown`, `closed`, `lastLines`, `lastW`
under `r.mu` (`live.go:51-59`). The sink is process-global, guarded by `liveSinkMu` (`ui.go:98-101`).
The bar moved out of `internal/cli/live_loop.go` to `internal/loop/bar.go` in the 2026-08
loop-engine extraction (its sink test `internal/cli/loopbar_sink_test.go` → `bar_test.go`);
`internal/loop` is a granted member of `uiPresentationOwners` precisely because this incremental
render is the one shape "return data, let the caller print it" cannot express
([[internal-import-dag]]).

**The trap: this subsystem was once deleted rather than debugged.** `9310911` ("keep terminal
output line-oriented") removed the whole live-bar stack — 463 lines — to make the stale frames stop;
`17cee39` reverted it and fixed the one-line real cause. The bar is a feature the human wants back
loudly. A glitch here is cursor arithmetic to root-cause, never a reason to remove the view. See
[[fix-the-bug-not-the-feature]] (the rule this incident produced) and
[[nonzero-progress-segments-stay-visible]] for the segment-visibility contract of the bar it paints.

## Changelog
- 2026-08-10 — created from `9310911`/`17cee39` and re-verified line by line against the CURRENT
  code after the loop-engine extraction (`internal/cli/live_loop.go` → `internal/loop/bar.go`). The
  VT-replay harness described in 17cee39 is NOT in the tree; recorded as a technique, not a test.
  New here, not in either commit message: the "wiped within 120ms" comment depends on the spinner
  ticker, which `COOP_SPINNER=0` removes.
