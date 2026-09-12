---
name: acp-waits-and-switches-are-visible
description: Announce automatic quota fallback and give each newly queued prompt its actual selected-account wait
scope: cli-output
sources: [internal/acpctl/control.go, internal/acpctl/held_status_test.go, internal/acpproxy/proxy.go, internal/acpproxy/held_status_test.go, internal/acpproxy/scripted_cancel_e2e_test.go]
check: go test ./internal/acpctl -run 'TestACPSelectedCoolingAccountStatus|TestACPControlAutomaticAccountRateLimitKeepsPolicy'
updated: 2026-09-12
---

# Show account fallback and every newly queued quota wait

Automatic quota fallback names the provider and account being tried even when Account stays Auto. Selecting a
cooling account discloses its reset. Every newly accepted prompt held behind that quota wait gets
a visible provider/account wait notice, even if an earlier prompt was cancelled and another wait was shown.

**Why:** On 2026-09-12 the user saw a silent hang after selecting a backup account. The trace
proved fallback ran but only updated the toolbar; a new held prompt bypassed controller admission
and received no wait status.

**How to apply:** Keep notices separate from prompt ownership. Promise automatic sending only
for a retained prompt. Use the selected account's reset, not another account's sooner reset.
Preserve cancellation, replay admission, and exactly-once delivery.

## Changelog
- 2026-09-12 — swept credential/preset quota rotation and replacement-held prompt admission; added target notices and presentation-only notification with controller/proxy/process cancellation regressions. No reset times, credentials, or network grants changed.
