# Approved output fixtures

One `.txt` per approved output state of the `coop tasks` family, named after the section of the CLI
design that approved it. Each file is the EXACT bytes the renderer must produce with color off.
A test compares the rendered output byte for byte; when a fixture and the code disagree, the fixture
is right and the code is wrong.

Source: the CLI design handoff reviewed on 2026-09-11, kept with its task under
`.agent/tasks/**/2026-09-10-make-network-inspection-destination-first-and-ex/reviewed-output/workflows-tasks.md`.
The numeric prefix is that document's review-item number, so a fixture traces back to the paragraph
that approved it.

Where the design's example carries host-specific data — a live PID, an absolute path, a wall-clock
time — the fixture keeps the approved SHAPE and substitutes the test's own fixed data. It never
invents a value the renderer could not have observed.

Changing a fixture means the approved design changed. Say so in the commit.
