# DRAFT — environment-sensitive test evidence

Git-backed tests must isolate both `GIT_CONFIG_GLOBAL` and `GIT_CONFIG_SYSTEM`; otherwise ambient
host configuration can make a fresh-repo test observe unrelated settings. When validating such a
fix, bypass Go's result cache with `-count=1` for the focused test and clear the test cache before
the literal project gate. A cached green result is not evidence that isolation works.
