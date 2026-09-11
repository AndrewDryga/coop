# Connect a fleet worker

`coop worker connect` connects one owner-private Coop session daemon to a compatible fleet
controller. It makes outbound mutual-TLS HTTPS requests; it does not open an inbound TCP port.
The controller sends versioned commands, not arbitrary shell commands. Provider work still runs
through the session daemon and its trusted policies, never as a connector-local fallback.

## Prepare the daemon and authority

Run the daemon and connector as the same operating-system user. Set up the session policies and
provider credentials described in the [session API guide](session-api.md#run), then run the daemon
under your process supervisor:

```bash
coop sessions serve \
  --state /var/lib/coop-sessions \
  --policies /etc/coop/session-policies.yaml \
  --socket /var/lib/coop-sessions/control.sock
```

From another terminal, check that exact socket and export the policy digests from the same file:

```bash
coop sessions doctor --socket /var/lib/coop-sessions/control.sock
coop sessions policies --policies /etc/coop/session-policies.yaml --json
```

Copy `policy_digests` and `policy_authority_digests` from that output into the worker configuration.
Keep the daemon's policies, worker advertisements and controller placement authority aligned.
The fleet operator supplies the worker/workspace identities, controller origin, trusted CA,
enrollment token and expected sandbox/repository advertisements. Do not derive those values from
the example or substitute a different digest that merely has the right length.

## Configure and enroll

Start with [worker.json](examples/worker.json), saving your completed configuration as
`/etc/coop/worker.json`. The example passes configuration validation, but its hostname, hashes,
policy name, paths and capacity are demonstration values, not deployment authority.

The format is strict JSON: no comments, unknown fields, trailing documents or environment-variable
expansion. Use literal absolute paths, not `~` or `$HOME` in JSON. In particular:

- `responder_url` is the controller's HTTPS origin, without an API endpoint, query or fragment.
  Serve its worker API routes directly, or rewrite internally at your proxy. The connector refuses
  HTTP redirects, including same-origin redirects; it does not forward credentials or transfer
  bytes to a redirect destination.
- `ca_file` contains exactly one trusted CA certificate in PEM format.
- `enrollment_token_file` is an owner-private regular file, at most 128 bytes. Its token must be
  32–128 bytes with no embedded whitespace. Obtain it through your controller's enrollment process;
  do not place the token in command arguments or the JSON file.
- `identity_file` starts absent. Do not precreate an empty file: the connector generates the key
  and saves its certificate bundle here during enrollment, with mode `0600`.
- `journal_dir` is persistent, owner-private storage, not a cache or disposable directory.
- `repositories`, `capabilities` and `capacity` are deployment advertisements. Populate them to
  match the controller's contract and actual worker resources. Coop only advertises its
  implementation capabilities — `repository-freshness` version `2` and
  `repository-source-selector` version `1` — after the running local daemon proves each one in
  `GET /v1/capabilities`; putting either in JSON cannot override that check, and a configured
  claim is removed. The two are versioned independently, so a partially upgraded fleet advertises
  only what each daemon actually supports and a controller can withhold source-selecting work
  from workers that do not have it.

Keep the configuration and enrollment file private (`0600`) and their containing directories
owned by the daemon/connector user. That user must be able to create the identity and journal,
and remove the enrollment token after use. Do not copy another worker's identity or journal.

A `create_session` command may carry a `source` selector beside its policy and digests
(`{"kind":"default"}`, `{"kind":"branch","name":…}`, `{"kind":"pull_request","number":…}` with an
optional `expected_head_commit`, or `{"kind":"commit","sha":…}`). The connector validates its
shape and bounds, forwards it unchanged to the daemon — which owns the authority decision — and
refuses a malformed one with `invalid_command` before any daemon call. A create fence carries the
same request, selector included, so it occupies the create's exact ledger identity. See
[session-api.md](session-api.md) for what each selector resolves to.

Run the connector under your process supervisor:

```bash
coop worker connect --config /etc/coop/worker.json
```

The first poll enrolls using the supplied token, saves the worker-owned identity, consumes the
local token file, then polls with its client certificate. Successful polls are quiet. Verify
enrollment and worker eligibility in your controller, not just by checking that the process lives.

`poll_interval_ms` must be 100–60000 and `request_timeout_ms` 100–300000. The example polls each
second with a 30-second request timeout. `renew_before_seconds` is 60–86400; omitted or zero
defaults to one hour. Choose a renewal window shorter than your controller's certificate lifetime.

## Restart and recovery

Stop the connector with `SIGINT` or `SIGTERM`. Preserve the identity file and the entire journal
directory across restarts, together with the same worker/workspace configuration. A restart loads
the existing identity rather than enrolling again; certificate renewal is automatic over mutual TLS.

Commands are journaled before execution. Unacknowledged terminal results are resent after restart.
Redelivery of a command with a recorded result reuses its receipt rather than executing it again;
reusing that command ID with a different payload is refused. An asynchronous
session creation can have its result acknowledged while creation is still running: the journal
retains its original operation identity until session activity can be bound. Event cursors advance
only after exact acknowledgements, so unacknowledged events replay and acknowledged events do not.
Do not prune journal files or keep only the `commands` directory when moving or backing up a worker.

If a review outlives its request, its uncertain transport receipt stays unchanged.
The existing `reconcile_operation` command returns the saved public operation/review
envelope once that exact review succeeds; pending or failed operations still return
their operation metadata. This reads the completed result and never reruns the gate.
The daemon and connector must both support completed-review lookup.

An unavailable daemon or controller is reported and retried; it does not authorize local execution
or receipt deletion. A malformed or expired saved identity fails closed instead of silently
re-enrolling, even if an enrollment token is present. Check file ownership, configured trust,
clock and controller renewal status, then follow the controller operator's identity-recovery
procedure. Do not delete the identity or journal as a routine reconnect fix.
