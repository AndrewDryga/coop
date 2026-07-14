# DRAFT — shell guards must preserve failure status

Shell discovery/count guards must capture and check each command's exit status. Suppressed stderr,
pipelines ending in `wc`, and process substitution can turn a failed `find` into an empty successful
result. For a completion guard, ambiguity must emit actionable diagnostics and fail closed.
