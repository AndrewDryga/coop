# Slugify

Add a `slug` subcommand to `tools.sh`.

Print the input lower-cased with spaces replaced by single dashes, trimmed.

## Acceptance criteria
- `sh ./tools.sh slug "  Hello There  "` prints `hello-there`
- `test.sh` gains a case for `slug` and still passes
- every subcommand that already worked still works

## Not done
- a subcommand that only handles the example above
- a change that makes `test.sh` pass by removing or weakening an existing case
