# Approved CLI output

Each `.txt` here is a COMPLETE approved transcript of one invocation — the exact bytes coop must
print: wording, order, capitalization, punctuation, indentation and blank lines. They arrived as
design decisions from the CLI content review (task
`2026-09-10-make-network-inspection-destination-first-and-ex`, `approved-output/`), and this copy
is the version the gate compares against.

The `TestApproved*` families use `assertApprovedOutput` in `approved_output_test.go` to compare
rendered output byte-for-byte with color off. Two values are legitimately substituted before the
comparison, because they are properties of the build and the project, not of the copy:

- the recorded version `v9.0.0-187-g1176bf4-dirty` becomes this build's version;
- the service-variant menus are rendered against a temporary project that really declares
  `postgres` and `redis` in `.agent/compose.yml`.

Everything else is literal. Angle brackets are help syntax, the leading blank line of an error
block is part of the block, and the six-space cause indentation and aligned `Usage:`/`Help:`/
`Example:` labels are the shared error contract.

**Changing a fixture is a design decision, not a test fix.** If the gate goes red here, the
renderer drifted — fix the renderer. New approved copy comes from the human who approved it
(record it in the owning task, then update the file and the renderer in the same commit).

**Help pages are not copied here.** `tools/gendocs` renders every command's page into
`docs/cli.md`, and `make docs-check` fails when a help string changes without regenerating it, so
a page's wording is reviewed in the `docs/cli.md` diff. A fixture would be a second copy of the
same string. The menus stay: their bytes depend on sign-in and services, which the manual's
reference form leaves out.

## Naming

Fixtures from the family files are named after the review item that approved them: the numeric
prefix is that document's item number (`01`–`03h` are the shared menu and input-error shapes from
`approved-output/`), so a fixture always traces back to the paragraph that approved it. A gap in
the numbers is usually an item whose copy is a help page, which `docs/cli.md` pins instead.
