# Collapse whitespace

Add a `squeeze` subcommand to `tools.sh`.

Print the input with each run of spaces collapsed to one.

## Acceptance criteria
- `sh ./tools.sh squeeze "a   b"` prints `a b`
- `test.sh` gains a case for `squeeze` and still passes
- every subcommand that already worked still works

## Not done
- a subcommand that only handles the example above
- a change that makes `test.sh` pass by removing or weakening an existing case
