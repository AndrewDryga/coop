# Make repeated imports safe

Use `entry_id` as a stable identity. Importing the same file twice must not duplicate rows. A duplicate ID with different memo or cents must fail and leave the whole batch unchanged; do not silently overwrite previous data. Add visible tests for both repeat and conflict. Preserve the summary CLI.

Not done: ignoring conflicting IDs or losing existing records.
