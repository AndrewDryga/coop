# Approved CLI output

Each `.txt` here is a COMPLETE approved transcript of one invocation — the exact bytes coop must
print: wording, order, capitalization, punctuation, indentation and blank lines. They arrived as
design decisions from the CLI content review (task
`2026-09-10-make-network-inspection-destination-first-and-ex`, `approved-output/`), and this copy
is the version the gate compares against.

`TestApprovedOutput` in `approved_output_test.go` renders each invocation with color off and
compares byte-for-byte. Two values are legitimately substituted before the comparison, because
they are properties of the build and the project, not of the copy:

- the recorded version `v9.0.0-187-g1176bf4-dirty` becomes this build's version;
- the service-variant menus are rendered against a temporary project that really declares
  `postgres` and `redis` in `.agent/compose.yml`.

Everything else is literal. Angle brackets are help syntax, the leading blank line of an error
block is part of the block, and the six-space cause indentation and aligned `Usage:`/`Help:`/
`Example:` labels are the shared error contract.

**Changing a fixture is a design decision, not a test fix.** If the gate goes red here, the
renderer drifted — fix the renderer. New approved copy comes from the human who approved it
(record it in the owning task, then update the file and the renderer in the same commit).

## Naming

Fixtures from the family files are named after the review item that approved them: the numeric
prefix is that document's item number (`35`–`46` are `reviewed-output/workflows-tasks.md`'s task
pages; `01`–`03h` are the shared menu and input-error shapes from `approved-output/`), so a fixture
always traces back to the paragraph that approved it.
