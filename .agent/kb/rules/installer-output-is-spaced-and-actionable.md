---
name: installer-output-is-spaced-and-actionable
description: installer progress, optional shell setup and final start/help examples use readable separate blocks
scope: cli-output
sources: [install.sh, install_test.go]
check: go test ./. -run TestInstallSetupOutcome
updated: 2026-10-01
---

# Keep installer output spaced and actionable

Group download, verification and installation progress under one clear heading. Mark only
completed checks as successful; never imply a signature was verified when Cosign was absent.
Separate optional Zsh setup prose from its copyable commands with blank lines, and separate
the image-build result from the sandbox checks. Finish with separate examples for starting
an agent and getting help. A failed build or doctor must never print the successful Done footer.

**Why:** the user requested this layout after showing the successful installer transcript.

## Changelog
- 2026-10-01 — swept install.sh and its mocked setup-outcome test. Corrected the progress block,
  Zsh paragraph spacing, build/doctor boundary and footer. Added one signed Zsh scenario to the
  existing success/failure test; shell setup remains instructions only, not a startup-file edit.
