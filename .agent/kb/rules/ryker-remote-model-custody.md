---
name: ryker-remote-model-custody
description: Ryker keeps raw model credentials and grants remote workers temporary proxy access
scope: security
sources: [internal/workerproto/model.go, internal/workerconnector/model.go, internal/box/controller_models.go, internal/box/native_remote.go, internal/sessionsvc/models.go, internal/cli/profiles.go]
check: "go test ./internal/box -run TestRemoteNativeRunKeepsProviderCredentialsAtController"
updated: 2026-11-27
---

# Keep remote model authority in Ryker

For Ryker-managed remote jobs, keep provider API keys, refresh tokens and
provider-issued access tokens in the Ryker controller. Issue temporary proxy
capabilities to worker VMs. Bind each capability to the worker, current job and
placement, run, provider, selected account epoch and allowed targets.

The operator selected this boundary on 2026-11-26. A worker can receive a
temporary credential issued by Ryker. It cannot receive the provider's raw
credential as its temporary credential.

Use the existing native provider contracts. The worker host retains its own
mTLS identity and relays requests through the controller. Only the run's
credential guard receives the temporary capability. The workload receives
public native selectors.

Keep grant responses outside ordinary command journals. A centrally bound job
must fail when its central authority is unavailable. Its local account files
cannot become a replacement authority.

Account setup must work without asking an operator to locate or upload private CLI
sign-in files. Use the provider’s supported sign-in for the selected account type.
Offer an offline, encrypted copy-and-paste handoff for a selected Coop account.
Move renewal ownership before emitting a renewable subscription bundle. Keep
the sealed reply available for retry to its original receiver.
Make any difference between subscription usage and separately billed API usage
visible when the operator chooses how to connect. Review this at the account UI.

## Changelog

- 2026-11-26 — recorded the operator's controller custody decision. Swept the
  five new worker relay sources. Raw provider credentials have no field in
  the worker grant contract. The focused test checks guard-only capability
  custody, public seeds and revocation. The central Ryker authority and full
  deployment matrix require their own integration proof.

- 2026-11-27 — real session-child qualification exposed the CLI's local-profile
  prerequisite. Controller-bound profile validation now proves the exact account
  through the private relay, without creating a worker-local login. The new CLI
  regression checks wrong-account and wrong-job denial as well as local-state absence.

- 2026-11-27 — traced both native upload branches to the Ryker account-access form.
  Codex requires one private JSON file and Claude requires two. The key entry
  and worker enrollment paths do not require those files. Recorded the setup
  correction for the next fleet revision.

- 2026-11-27 — reviewed Coop’s four account adapters and Ryker’s two supported
  providers. The operator chose to move a subscription’s renewal ownership.
  Checked concurrent account replacement, interrupted clipboard transfer and
  two sources exporting to one destination account in the transfer tests.
