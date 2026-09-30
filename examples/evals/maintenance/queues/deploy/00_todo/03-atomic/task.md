# Replace the output atomically

Write a complete output in a temporary file beside the destination, then rename it into place. Any input, validation, or write failure must leave the prior output unchanged and clean up the temporary file. Keep the existing format and all earlier behavior. Add a failure-path regression to `test.sh`.

Not done: writing a partial destination before validation finishes or leaving temporary files behind.
