# Import a batch all-or-nothing

Validate and import one CSV file as one transaction. If any row is invalid, return a nonzero exit and leave existing database rows unchanged. The database file may exist already. Preserve the summary CLI and add a visible rollback test.

Not done: committing each good row before a later bad row fails.
