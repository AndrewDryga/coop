# Preserve configuration values and paths with spaces

`deploy.sh` currently expands its input path unsafely. Make it accept a config filename containing spaces and preserve values containing spaces. Reject a missing config or missing required key with a nonzero exit, without printing a success-looking output. Keep the existing `NAME`/`PORT`/`LOG` format and baseline behavior. Add a visible regression to `test.sh`.

Not done: hard-coding the example path or weakening the baseline test.
