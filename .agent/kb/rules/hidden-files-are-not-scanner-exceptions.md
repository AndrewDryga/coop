---
name: hidden-files-are-not-scanner-exceptions
description: .coopignore hides files from agents; only exact reviewed .coopsecretsignore findings dismiss scanner warnings
scope: security
sources: [internal/cli/checksecrets.go, internal/cli/checksecrets_test.go, internal/box/secrets.go, internal/secretscan/exceptions.go, internal/secretscan/exceptions_test.go]
check: none
updated: 2026-09-11
---

# Separate box hiding from scanner exceptions

`.coopignore` controls what agents can read, not which secret findings the user has dismissed.
`coop check-secrets` must scan Git-tracked and non-Git-ignored files selected for scanning even
when box hiding covers them. Hiding a credential from an agent does not prevent committing it.

Use the existing `.coopsecretsignore` for reviewed exact findings. A dismissed fixture finding
must not suppress a changed credential value or another finding in the same file. Keep ordinary
box hiding and normal scan scope intact; do not add commands, formats, or approval prompts.

**Why:** the user confirmed that false positives should be easy to silence, then approved the
two-file distinction: hide a real key from the agent while still warning if Git tracks it;
dismiss a known fixture finding but check a changed value again.

**How to apply:** cover hidden tracked files (including tracked files matching `.gitignore`),
hidden untracked/non-Git-ignored files, nested hide rules, and exact exception identity. Preserve
the existing exception boundary: scanner dismissals do not grant permission to export secrets
through fork merges, checkpoints, or session output.

## Changelog

- 2026-09-11 — recorded the approved S9 design. Swept the five source files above: box hiding
  and exact-finding exceptions are separate helpers, but checksecrets.go skips `.coopignore`
  matches and TestCheckSecretsReportsNameShadowedCommitCandidates requires that silence.
  Those implementation/test violations are queued as S9 in
  2026-09-11-fix-defects-found-by-the-full-coop-feature-audit. No regression enforces this
  separation yet; check remains none until implementation.
