# Validate the port override

Accept `APP_PORT` only when it is a decimal integer from 1 to 65535. An invalid override must fail without modifying an existing output. Keep the config port as the default and preserve all other output fields. Add valid and invalid override cases to `test.sh`.

Not done: silently ignoring an invalid override or truncating the output before reporting an error.
