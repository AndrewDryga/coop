---
name: secret-finding-identity
description: how a secret finding is named across runs (fp-v1 fingerprints), what .coopsecretsignore may and may not excuse, and why exceptions stop at check-secrets
subsystem: secretscan
sources: [internal/secretscan/secretscan.go, internal/secretscan/exceptions.go, internal/cli/checksecrets.go, internal/box/secretscan.go, internal/forkctl/merge.go, internal/forkctl/isolated_candidate.go, internal/gitdiff/gitdiff.go]
updated: 2026-11-06
---

A reviewed false positive is excused by NAMING the exact finding, so the detectors never learn to
be quieter. The name is `fp-v1:<sha256>` over a length-prefixed encoding of three things
(`Fingerprint`, secretscan.go): the repository-relative path, the stable detector id, and the
complete matched credential material. What is deliberately absent is as load-bearing as what is
present:

- **No line number.** Inserting a line above a finding must not invalidate the entry someone wrote.
- **Nothing machine-specific** — no absolute path, no host key. The same repository in a second
  checkout produces the same ids, which is what lets `.coopsecretsignore` be committed.
- **The material, not the marker.** A private key is fingerprinted BEGIN-through-END, because every
  key in a file shares the BEGIN line; an unterminated block falls back to the whole file, so the
  id changes on any edit (re-reports rather than hides).

One credential is one finding. `secretPatterns` order is precedence: on each line a provider match
that overlaps a span an earlier pattern already reported is dropped, so an `sk-ant-…` key (also
`sk-…`-shaped) is one `anthropic_api_key` finding, never a second `openai_api_key` one. A broader
shape goes after the specific one it overlaps. Before 2026-11-03 such a key reported both; a saved
`openai_api_key` entry for it now simply matches nothing.

Detector ids (`DetectorOpenAIAPIKey` and friends) are a compatibility surface: rename one and every
saved exception for it silently stops applying. Labels above them are presentation and may be
reworded. A fingerprint is a checksum, not encryption — it identifies a finding, it does not make
the credential it was computed from safe to publish.

`ScanSecrets(content)` stays the pure detector every other consumer shares (fork merge's PolicyScan,
checkpoint upload, session redaction) and returns findings with an EMPTY fingerprint — identity
needs a path those callers do not supply. `ScanFile(path, content)` is the check-secrets form.
Exceptions are applied in `cmdCheckSecrets` after pure detection and NOWHERE else: an in-repository
note is not permission to move a credential out of the repository.

A fork is judged only on what it adds. Fork merge (`PolicyScan`) and isolated publication
(`validateIsolatedHistory`) still scan the whole file, so line numbers and a private key's
BEGIN-to-END block are unchanged, but report a finding only on a line the fork adds: every line of a
file it adds, else the new side of a `-U0` diff against the merge base, or for isolated publication
against each commit's first parent (`gitdiff.AddedLines`). A credential the project's copy already
held never holds up a fork that edits or renames its file. check-secrets still judges whole files.

The assignment heuristic reads a line as a string literal writes it: `\n`, `\r` and `\t` are
whitespace (`literalWhitespace`), so `"URL=%s\nAPI_KEY=%s\n"` assigns `%s` to `API_KEY` and does not
fire, where it used to read `nAPI_KEY`. A finding whose value ran across such an escape now hashes the
shorter value, so its old exception re-reports rather than hides.

`.coopignore` never dismisses a commit-candidate finding. `scanVisibleTree` uses the
shared shadow decision only to label hidden candidates and exclude hidden files
outside the commit-candidate set. Root/nested hide rules, tracked files also matched
by `.gitignore`, and untracked non-ignored files follow the same selection rule.
Box hiding remains independent of reviewed exact-finding exceptions.
The shared shadow decision includes hidden ancestor directories: their committable
descendants are still scanned and labeled, while ignored noncandidates are skipped
and do not inflate the visible-ignored count. Fork policy also warns on changed
files below a hidden parent, without changing content detection or exceptions.
Its warning asks for review and removal of real credentials from the fork, not box
hiding or scanner exceptions. The warning carries path, line and kind, never raw material.

Traps:
- A file coop could not READ is not a skip. `readScannable` separates `scanSkipped` (binary,
  oversized, non-regular — quiet and deliberate) from `scanUnreadable`, and any unreadable file
  fails the scan even when every finding it did reach was already excused. "No secrets found" over
  a hole is the failure mode this exists to prevent.
- `LoadExceptions` treats unreadable, non-regular and oversized files as ERRORS, never as "no
  exceptions": coop cannot tell what the person decided, so it stops.
- The exception file is scanned like any other project file; its comments are not a hiding place.

## Changelog
- 2026-11-06: fork merge and isolated publication report only findings on lines the fork adds; the
  assignment heuristic reads a literal's `\n`, `\r` and `\t` as whitespace (task
  2026-11-05-flag-only-secrets-a-fork-adds-not-lines-already).
- 2026-11-03: overlapping provider matches report once, by the earlier pattern
  (TestScanSecretsReportsEachTokenOnce).
- 2026-09-13: corrected fork warning advice; synthetic visible/hidden fixtures retain
  non-forced merge refusal and prove removing the credential clears only its content finding.
- 2026-09-13: verified ancestor-directory hiding across scanner modes and ignored
  counts, with a fork policy regression for hidden and public siblings.
- 2026-09-13: removed the scanner's unconditional .coopignore exemption; verified
  hidden candidate selection, unchanged mount protection and exact exceptions.
- 2026-09-11: created with the fingerprint contract, the ScanSecrets/ScanFile split, and the
  incomplete-scan rule. Verified against secretscan.go, exceptions.go and checksecrets.go.
