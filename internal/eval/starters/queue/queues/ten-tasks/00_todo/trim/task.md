# Trim whitespace

Add a `trim` subcommand to `tools.sh`.

Print the input with leading and trailing whitespace removed.

## Acceptance criteria
- `sh ./tools.sh trim "  hi  "` prints `hi`
- `test.sh` gains a case for `trim` and still passes
- every subcommand that already worked still works

## Not done
- a subcommand that only handles the example above
- a change that makes `test.sh` pass by removing or weakening an existing case
