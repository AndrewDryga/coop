---
name: secret-finding-identity
description: how a secret finding is named across runs (fp-v1 fingerprints), what .coopsecretsignore may and may not excuse, and why exceptions stop at check-secrets
subsystem: secretscan
sources: [internal/secretscan/secretscan.go, internal/secretscan/exceptions.go, internal/cli/checksecrets.go, internal/box/secretscan.go]
updated: 2026-09-11
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

Detector ids (`DetectorOpenAIAPIKey` and friends) are a compatibility surface: rename one and every
saved exception for it silently stops applying. Labels above them are presentation and may be
reworded. A fingerprint is a checksum, not encryption — it identifies a finding, it does not make
the credential it was computed from safe to publish.

`ScanSecrets(content)` stays the pure detector every other consumer shares (fork merge's PolicyScan,
checkpoint upload, session redaction) and returns findings with an EMPTY fingerprint — identity
needs a path those callers do not supply. `ScanFile(path, content)` is the check-secrets form.
Exceptions are applied in `cmdCheckSecrets` after pure detection and NOWHERE else: an in-repository
note is not permission to move a credential out of the repository.

Traps:
- A file coop could not READ is not a skip. `readScannable` separates `scanSkipped` (binary,
  oversized, non-regular — quiet and deliberate) from `scanUnreadable`, and any unreadable file
  fails the scan even when every finding it did reach was already excused. "No secrets found" over
  a hole is the failure mode this exists to prevent.
- `LoadExceptions` treats unreadable, non-regular and oversized files as ERRORS, never as "no
  exceptions": coop cannot tell what the person decided, so it stops.
- The exception file is scanned like any other project file; its comments are not a hiding place.

## Changelog
- 2026-09-11: created with the fingerprint contract, the ScanSecrets/ScanFile split, and the
  incomplete-scan rule. Verified against secretscan.go, exceptions.go and checksecrets.go.
