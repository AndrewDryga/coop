# Never fix a bug by deleting the feature

When a feature misbehaves (a rendering glitch, a race, a flaky path), the fix is to root-cause
the misbehavior and repair it — not to remove or degrade the feature so the bug has nowhere to
live. Deleting the feature is the one "fix" that is always wrong without the human's explicit
sign-off: it trades a defect the user wants fixed for a regression they never asked for.

**Why:** the loop's live progress bar leaked stale frames into scrollback whenever Ctrl-C was
pressed. The real cause was one terminal detail: the kernel echoes `^C` at the cursor, which sat
at the bar's end-of-line, filled the last column, wrapped, and desynced the region's erase math.
Instead of that one-line diagnosis, a "fix" (`9310911`) deleted the whole live-bar subsystem
(463 lines) and called the loss "line-oriented output". The human wanted the bar; it took a
revert plus the actual fix (park the cursor at column 0 after every paint) to recover.

**How to apply:**
- Reproduce and name the mechanism of the defect before touching code (`/investigate`); if you
  can't state the root cause in a sentence, you aren't ready to fix anything.
- If, after root-causing, removal still looks like the right call, that's a one-way door:
  `coop tasks block` it / ask the human — never ship removal as a bug fix on your own.
- Narrowing WHERE a feature runs (e.g. the bar's Warp/no-TTY guard) is fine when the root cause
  is a platform that can't host it — but the feature keeps working everywhere it can.

See also [[destructive-confirm-gate]] (destruction needs a human gate — that spirit applies to
code, too).
