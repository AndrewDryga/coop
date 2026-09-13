package sessionsvc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/session"
)

// sessionNetworkEventVersion is the schema of the `network` event payload.
const sessionNetworkEventVersion = 1

// sessionNetworkBinding is what session creation freezes on the immutable session row. A filtered
// session carries the owner-keyed snapshot fingerprint its every run must match plus the host
// setup record that qualified it; open and offline sessions carry the mode and nothing else,
// because nothing was captured for them.
type sessionNetworkBinding struct {
	Mode          egress.Mode
	Fingerprint   string
	Qualification string
}

// admitSessionNetwork resolves this session's network posture ONCE, while it is being created,
// and freezes the policy its runs will enforce. Later runs load the captured snapshot; they never
// admit again, so an approval or configuration edit that lands mid-session can produce a visible
// denial but never a wider policy.
//
// It is the only place a session's network authority is decided, which is why the operator policy
// arrives here as a value and the repository's own requests arrive as requests.
func (s *Service) admitSessionNetwork(policy Policy, workspace, forkName string, companions []session.CompanionRepository) (sessionNetworkBinding, error) {
	if policy.Mode == agents.ModeBare {
		// No project, so no remembered approval to admit against and nothing to capture: the
		// posture is the policy's own written one, open when it wrote none. Filtered was
		// refused when the policy loaded.
		return sessionNetworkBinding{Mode: policy.Egress.resolvedMode()}, nil
	}
	if s.testAdmitNetwork != nil {
		return s.testAdmitNetwork(policy, workspace, forkName)
	}
	if s.sourceCfg == nil {
		// No host configuration means no credentials, no runtime and no authority root to
		// resolve against. A policy that asked for a posture cannot be honored silently.
		if policy.Egress.configured() && policy.Egress.resolvedMode() != egress.Open {
			return sessionNetworkBinding{}, errors.New("restricted networking requires host configuration")
		}
		return sessionNetworkBinding{Mode: egress.Open}, nil
	}
	if err := s.ensureRuntimeForNetwork(policy); err != nil {
		return sessionNetworkBinding{}, err
	}
	mode, capture, err := box.AdmitSessionNetwork(s.sourceCfg, s.rt,
		sessionNetworkAdmissionSpec(s.sourceCfg, policy, workspace, forkName, companions),
		box.SessionNetworkAdmission{
			Mode:               sessionPolicyEgressMode(policy),
			Rules:              policy.Egress.Rules,
			ExportDestinations: policy.Egress.ExportDestinations,
			OmitMCP:            policy.OmitMCP,
		})
	if err != nil {
		return sessionNetworkBinding{}, err
	}
	defer capture.Close()
	if capture == nil {
		return sessionNetworkBinding{Mode: mode}, nil
	}
	return sessionNetworkBinding{Mode: mode, Fingerprint: capture.Fingerprint, Qualification: capture.QualificationID}, nil
}

// PolicyNetwork is one policy's effective network reach on THIS host: the posture its sessions
// run under, and — for a filtered policy — the owner-keyed fingerprint of the exact rules a create
// would freeze. A caller cannot compute that fingerprint from the policy file: the project's
// remembered approval, the provider core bundles and the trusted MCP hosts all feed it. So the
// daemon publishes it and a placement pins the value it was authorized against.
//
// An open or offline policy has no fingerprint, because nothing is captured for one.
type PolicyNetwork struct {
	Mode        egress.Mode `json:"mode"`
	Fingerprint string      `json:"fingerprint,omitempty"`
}

// ResolvePolicyNetwork compiles what this policy reaches without writing any host state: no
// approval, no published snapshot, no owner key. The daemon calls it once per policy when it loads
// them and again on a fenced create, so an approval edited on the host between those two moments
// becomes an explicit refusal instead of a session running under rules nobody pinned.
func ResolvePolicyNetwork(cfg *config.Config, policy Policy) (PolicyNetwork, error) {
	network, _, err := ResolvePolicyNetworkSnapshot(cfg, policy)
	return network, err
}

// ResolvePolicyNetworkSnapshot is ResolvePolicyNetwork plus the compiled snapshot behind it, so a
// reader can be shown the grants this policy actually resolves to — the provider bundles and the
// project's approved rules together — rather than re-deriving them from the policy YAML, which
// would omit whatever the project itself contributed. The PolicyNetwork is byte-identical to what
// ResolvePolicyNetwork returns: this is evidence for a human view, not a new wire field.
func ResolvePolicyNetworkSnapshot(cfg *config.Config, policy Policy) (PolicyNetwork, egress.Snapshot, error) {
	if policy.Mode == agents.ModeBare {
		// Nothing host-side feeds a bare policy's reach — see admitSessionNetwork.
		return PolicyNetwork{Mode: policy.Egress.resolvedMode()}, egress.Snapshot{}, nil
	}
	if cfg == nil {
		// No host configuration means no credentials, no runtime and no authority root to
		// resolve against — the same answer admission gives.
		if policy.Egress.configured() && policy.Egress.resolvedMode() != egress.Open {
			return PolicyNetwork{}, egress.Snapshot{}, errors.New("restricted networking requires host configuration")
		}
		return PolicyNetwork{Mode: egress.Open}, egress.Snapshot{}, nil
	}
	// The session's own workspace, fork and companions do not exist yet, and none of them reach
	// the compile: they describe what a launch MOUNTS, while the fingerprint is compiled from the
	// project's approval, the policy's rules, the provider bundles and the shared MCP hosts. The
	// policy's repository stands in for them, which is the project the approval belongs to anyway.
	mode, snapshot, err := box.ResolveSessionNetworkSnapshot(cfg,
		sessionNetworkAdmissionSpec(cfg, policy, policy.Repository, "", nil),
		box.SessionNetworkAdmission{
			Mode:               sessionPolicyEgressMode(policy),
			Rules:              policy.Egress.Rules,
			ExportDestinations: policy.Egress.ExportDestinations,
			OmitMCP:            policy.OmitMCP,
		})
	if err != nil {
		return PolicyNetwork{Mode: mode}, egress.Snapshot{}, err
	}
	return PolicyNetwork{Mode: mode, Fingerprint: snapshot.Fingerprint}, snapshot, nil
}

// resolvePolicyNetwork is the daemon's own resolution of one policy, fresh from host state.
func (s *Service) resolvePolicyNetwork(policy Policy) (PolicyNetwork, error) {
	if s.testResolveNetwork != nil {
		return s.testResolveNetwork(policy)
	}
	return ResolvePolicyNetwork(s.sourceCfg, policy)
}

// resolvePolicyNetworks resolves every policy the daemon is about to serve. A policy whose network
// cannot be resolved — no approval for its project, no host setup, a rule this release cannot
// enforce — refuses the whole load with its own reason, exactly as an unparsable or credential-less
// policy does: serving it unfenced would advertise a reach nobody could pin.
func resolvePolicyNetworks(policies map[string]Policy, cfg *config.Config) (map[string]PolicyNetwork, error) {
	networks := make(map[string]PolicyNetwork, len(policies))
	for _, name := range slices.Sorted(maps.Keys(policies)) {
		network, err := ResolvePolicyNetwork(cfg, policies[name])
		if err != nil {
			return nil, fmt.Errorf("policy %q: %w", name, err)
		}
		// A restricted policy that wrote no posture inherits the project's remembered one, and
		// the restricted profile is not qualified under a filtered gateway: refuse the load, as
		// an explicit `egress.mode: filtered` on the same policy already was.
		if policies[name].Mode.Restricted() && network.Mode == egress.Filtered {
			return nil, fmt.Errorf("policy %q: resolves to filtered networking on this host, which a %s session is not qualified under — set egress.mode to open or none", name, policies[name].Mode)
		}
		networks[name] = network
	}
	return networks, nil
}

// sessionPolicyEgressMode returns the operator's EXPLICIT posture, or "" when the policy file
// wrote no `egress:` block at all. The built-in open default is not an explicit request to widen
// access: a policy that stays silent lets the remembered project posture decide, exactly as a
// direct `coop claude` in that project would.
func sessionPolicyEgressMode(policy Policy) egress.Mode {
	if !policy.Egress.configured() {
		return ""
	}
	return policy.Egress.resolvedMode()
}

// ensureRuntimeForNetwork binds the container runtime admission needs to match a host setup
// record against. Only a filtered policy needs it, so an open session still creates no runtime
// dependency it did not have before.
func (s *Service) ensureRuntimeForNetwork(policy Policy) error {
	if sessionPolicyEgressMode(policy) != egress.Filtered {
		return nil
	}
	return s.ensureRunner()
}

// sessionNetworkAdmissionSpec is the launch the daemon is about to authorize, described exactly as
// the ACP child will build it — the fork workspace it mounts, the companions beside it, and every
// rung of the target ladder, so a rotation later in the session cannot reach an endpoint the
// capture never froze.
//
// PolicyRepo is the session's REPOSITORY, not its workspace: network approval and the remembered
// posture belong to the project an operator approved, and a workspace is agent-writable.
func sessionNetworkAdmissionSpec(cfg *config.Config, policy Policy, workspace, forkName string, companions []session.CompanionRepository) box.RunSpec {
	repositories := make([]box.CompanionRepository, 0, len(companions))
	for _, companion := range companions {
		repositories = append(repositories, box.CompanionRepository{
			Name: companion.Name, HostPath: companion.Workspace, BaseCommit: companion.BaseCommit,
		})
	}
	lead := ""
	if len(policy.Targets) != 0 {
		lead = policy.Targets[0].Provider
	}
	return box.RunSpec{
		Repo: workspace, Workdir: workspace, PolicyRepo: policy.Repository,
		RepoReadOnly: policy.RepositoryReadOnly, ForkName: forkName,
		Agent: lead, Peers: append([]agents.Target(nil), policy.Targets...),
		NetworkClient:         egress.ClientACP,
		Homes:                 true,
		Network:               cfg.Network,
		Cache:                 cfg.Cache,
		CompanionRepositories: repositories,
	}
}

// networkChildEnvironment is the capture one ACP child receives, or nothing. The daemon is the
// child's HOST parent and scrubs every COOP_* from the environment it builds, so this is the only
// way a session box can be launched filtered — and nothing inside a box can write it.
func networkChildEnvironment(bound session.Session, runID string) ([]string, error) {
	mode := bound.NetworkMode
	if mode == "" || mode == string(egress.Open) {
		return nil, nil
	}
	if _, err := egress.ParseMode(mode); err != nil {
		return nil, fmt.Errorf("session network mode: %w", err)
	}
	env := []string{"COOP_EGRESS=" + mode}
	if mode != string(egress.Filtered) {
		return env, nil
	}
	capture, err := box.SessionNetworkCapture{
		Project: bound.Repository, Fingerprint: bound.NetworkFingerprint,
		Qualification: bound.NetworkQualification, SessionID: bound.ID, AttemptID: runID,
	}.Encode()
	if err != nil {
		return nil, err
	}
	return append(env, box.SessionNetworkCaptureEnv+"="+capture), nil
}

// sessionEvidence opens the owner-private execution registry read-only. It creates no owner key
// and publishes nothing, so a read of a session that never ran filtered is inert.
func sessionEvidence(bound session.Session) (*networkstate.Evidence, error) {
	root, err := box.NetworkStatePath()
	if err != nil {
		return nil, err
	}
	exposed := []string{bound.Repository, bound.Workspace}
	for _, companion := range bound.Companions {
		exposed = append(exposed, companion.Workspace)
	}
	return networkstate.OpenEvidence(root, exposed)
}

// sessionExecutions lists every network run this session owns, newest launch last. The page's own
// incompleteness travels with it: a truncated inventory can never prove that no other run exists.
func sessionExecutions(evidence *networkstate.Evidence, sessionID string) ([]networkstate.Execution, bool, error) {
	var records []networkstate.Execution
	complete, cursor := true, ""
	for {
		page, err := evidence.Executions(cursor)
		if err != nil {
			return nil, false, err
		}
		if page.Incomplete {
			complete = false
		}
		for _, summary := range page.Executions {
			if summary.SessionID != sessionID {
				continue
			}
			record, err := evidence.Execution(summary.ID)
			if err != nil {
				complete = false
				continue
			}
			records = append(records, record)
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	slices.SortFunc(records, func(a, b networkstate.Execution) int {
		if order := a.StartedAt.Compare(b.StartedAt); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return records, complete, nil
}

// verifySessionNetworkRuns proves that every run registered against this session enforced the
// session's OWN captured policy. The daemon writes the fingerprint into the immutable session row
// at creation and the child cannot influence what its execution recorded, so a disagreement means
// a run enforced authority this session was never granted — which fails the turn rather than
// being reported after the fact.
// It names the offending run so the refusal and the event that reports it point at the same
// evidence.
func verifySessionNetworkRuns(bound session.Session, records []networkstate.Execution) (string, error) {
	for _, record := range records {
		if record.Snapshot.PolicyFingerprint == bound.NetworkFingerprint {
			continue
		}
		return record.ID, fmt.Errorf(
			"network run %s enforced policy %s, but session %s is bound to %s",
			record.ID, record.Snapshot.PolicyFingerprint, bound.ID, bound.NetworkFingerprint)
	}
	return "", nil
}

// latestSessionRun returns the most recently started run for one attempt. A cold turn has exactly
// one; a session's warm attempt id is stable, so the child that just exited is the newest run
// carrying it and the earlier ones were reported when they ended.
func latestSessionRun(records []networkstate.Execution, attemptID string) (networkstate.Execution, bool) {
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].AttemptID == attemptID {
			return records[i], true
		}
	}
	return networkstate.Execution{}, false
}

// sessionNetworkReport folds one sealed run's retained evidence into the bounded summary the
// session event carries. An unsealed run is still being amended, so it reports nothing.
func sessionNetworkReport(record networkstate.Execution, exportDestinations bool) (box.NetworkReport, bool) {
	if record.Receipt == nil {
		return box.NetworkReport{}, false
	}
	receipt, err := record.Receipt.Project(exportDestinations)
	if err != nil {
		return box.NetworkReport{}, false
	}
	report := box.NetworkRunReport(record.ID, receipt.Snapshot)
	return report, !report.Quiet()
}

// sessionNetworkEventPayload is the `network` event body: bounded by construction — refusals are
// grouped and capped, alerts are capped — so a box that generates a thousand denied names costs
// one event with an omitted count, not a thousand.
type sessionNetworkEventPayload struct {
	Version   int                    `json:"version"`
	RunID     string                 `json:"run_id"`
	Denials   []sessionNetworkDenial `json:"denials,omitempty"`
	Omitted   int                    `json:"omitted_destinations,omitempty"`
	Alerts    []string               `json:"alerts,omitempty"`
	Allowed   string                 `json:"allowed_traffic"`
	Truncated bool                   `json:"detail_truncated,omitempty"`
	Evidence  string                 `json:"evidence_id,omitempty"`
}

type sessionNetworkDenial struct {
	Destination string `json:"destination"`
	Basis       string `json:"basis"`
	Count       int    `json:"count"`
}

func sessionNetworkPayload(report box.NetworkReport) ([]byte, error) {
	payload := sessionNetworkEventPayload{
		Version: sessionNetworkEventVersion, RunID: report.RunID, Omitted: report.Omitted,
		Alerts: report.Alerts, Allowed: report.Allowed, Truncated: report.Truncate, Evidence: report.Event,
	}
	for _, denial := range report.Denials {
		payload.Denials = append(payload.Denials, sessionNetworkDenial{
			Destination: denial.Destination, Basis: denial.Basis, Count: denial.Count,
		})
	}
	return json.Marshal(payload)
}

// sessionSnapshotExport reports whether this session's captured policy opted its projections into
// concrete destination names. A session with no capture never exports one: there is nothing to
// export, and defaulting to disclosure is the wrong direction to be wrong in.
func sessionSnapshotExport(bound session.Session) bool {
	policy, err := loadSessionSnapshot(bound)
	return err == nil && policy.ExportDestinations
}

// sessionNetworkOutcome closes the loop on one filtered child launch. It proves that every run
// registered against this session enforced the session's own frozen policy — a mismatch is a
// custody failure and fails the turn — and, once the child has actually exited, publishes what
// that run could not reach on the session's event stream.
//
// A read failure is an observation gap, not a violation: enforcement already happened at the box
// boundary, so losing sight of the evidence warns rather than destroying a finished turn.
func (r *sessionTurnRunner) sessionNetworkOutcome(bound session.Session, turnID, attemptID string, childExited bool) error {
	if bound.NetworkMode != string(egress.Filtered) || bound.NetworkFingerprint == "" {
		return nil
	}
	records, complete, err := r.sessionNetworkRecords(bound)
	if err != nil {
		r.warnNetwork(bound, err)
		return nil
	}
	if mismatch, err := verifySessionNetworkRuns(bound, records); err != nil {
		// The turn fails on this, but a caller watching the stream rather than the turn's
		// result still has to learn that a run enforced authority this session never had.
		r.publishNetworkEvent(bound, turnID, box.NetworkReport{RunID: mismatch, Alerts: []string{err.Error()}})
		return acpFailure(sessionACPProcessError, err.Error())
	}
	if !complete {
		// A partial inventory cannot prove the absence of a mismatching run: the record this
		// read skipped is exactly where one would hide. Fail the turn instead of certifying it.
		incomplete := fmt.Errorf("network evidence for this session is incomplete; session %s cannot be certified against its captured policy", bound.ID)
		r.publishNetworkEvent(bound, turnID, box.NetworkReport{Alerts: []string{incomplete.Error()}})
		return acpFailure(sessionACPProcessError, incomplete.Error())
	}
	if !childExited || attemptID == "" {
		return nil
	}
	record, ok := latestSessionRun(records, attemptID)
	if !ok {
		return nil
	}
	report, worth := sessionNetworkReport(record, sessionSnapshotExport(bound))
	if !worth {
		return nil
	}
	r.publishNetworkEvent(bound, turnID, report)
	return nil
}

// publishNetworkEvent puts one run's networking outcome on the session's durable stream. It is
// best effort by design: the boundary already enforced the policy, so failing to narrate that is
// a reporting gap, never a reason to destroy a finished turn.
func (r *sessionTurnRunner) publishNetworkEvent(bound session.Session, turnID string, report box.NetworkReport) {
	payload, err := sessionNetworkPayload(report)
	if err != nil {
		r.warnNetwork(bound, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionACPCleanupTimeout)
	defer cancel()
	if _, err := r.store.AppendEvent(ctx, session.AppendEventRequest{
		SessionID: bound.ID, TurnID: turnID, Type: session.EventNetwork,
		Version: sessionNetworkEventVersion, Payload: payload,
	}); err != nil {
		r.warnNetwork(bound, err)
	}
}

// sessionNetworkRecords lists every network run this session owns, closing the read-only
// evidence handle before the caller does anything with the answer. complete is false when the
// inventory could not be read whole — the one case where "no mismatching run" proves nothing.
func (r *sessionTurnRunner) sessionNetworkRecords(bound session.Session) ([]networkstate.Execution, bool, error) {
	if r.testNetworkExecutions != nil {
		return r.testNetworkExecutions(bound)
	}
	evidence, err := sessionEvidence(bound)
	if err != nil {
		return nil, false, err
	}
	defer evidence.Close()
	return sessionExecutions(evidence, bound.ID)
}

func (r *sessionTurnRunner) warnNetwork(bound session.Session, cause error) {
	r.host.warnf("session %s networking evidence is unavailable: %s",
		bound.ID, sessionACPBoundedDetail("cause", cause.Error()))
}

func loadSessionSnapshot(bound session.Session) (egress.Snapshot, error) {
	if bound.NetworkMode != string(egress.Filtered) || bound.NetworkFingerprint == "" {
		return egress.Snapshot{}, errors.New("session has no captured network policy")
	}
	root, err := box.NetworkStatePath()
	if err != nil {
		return egress.Snapshot{}, err
	}
	exposed := []string{bound.Repository, bound.Workspace}
	for _, companion := range bound.Companions {
		exposed = append(exposed, companion.Workspace)
	}
	store, err := networkstate.OpenExisting(root, exposed)
	if err != nil {
		return egress.Snapshot{}, err
	}
	defer store.Close()
	return store.LoadSnapshot(bound.Repository, bound.NetworkFingerprint)
}

// SessionNetworkDTO is the live read: what this session may reach, and what its newest run has
// actually been observed doing. Destinations appear only when the operator policy opted in;
// otherwise this is counts, health and reasons, which is what a remote client is entitled to.
type SessionNetworkDTO struct {
	SessionID   string                   `json:"session_id"`
	Mode        string                   `json:"mode"`
	Fingerprint string                   `json:"fingerprint,omitempty"`
	Requested   []string                 `json:"requested"`
	Effective   []string                 `json:"effective"`
	Current     SessionNetworkCurrentDTO `json:"current"`
	Alerts      []networkview.Alert      `json:"alerts"`
	Projection  string                   `json:"projection"`
}

// SessionNetworkCurrentDTO summarizes the newest run's retained observation. Status is the one
// field always present: "no run yet" is a different fact from "observed nothing", and a stale
// sample is not a closed connection.
type SessionNetworkCurrentDTO struct {
	Status       string                       `json:"status"`
	RunID        string                       `json:"run_id,omitempty"`
	Availability string                       `json:"availability,omitempty"`
	Scope        string                       `json:"scope,omitempty"`
	AsOf         *time.Time                   `json:"as_of,omitempty"`
	Counters     *networkview.Counters        `json:"counters,omitempty"`
	Live         *networkstate.CurrentNetwork `json:"live,omitempty"`
	Cleanup      string                       `json:"cleanup_outcome,omitempty"`
	Sealed       bool                         `json:"sealed,omitempty"`
}

// SessionNetworkReceiptDTO is the GC-independent aggregate across every run this session owned.
// Availability is explicit: an open session has no receipt because nothing captured a policy for
// it, which is not the same as a filtered session that has not run yet.
type SessionNetworkReceiptDTO struct {
	SessionID   string                      `json:"session_id"`
	Mode        string                      `json:"mode"`
	Fingerprint string                      `json:"fingerprint,omitempty"`
	Available   bool                        `json:"available"`
	Reason      string                      `json:"reason,omitempty"`
	Receipt     *networkview.SessionReceipt `json:"receipt,omitempty"`
}

// SessionNetwork answers "what can this session reach, and what is it doing right now" from
// retained evidence alone. It probes no gateway, reads no runtime and publishes nothing.
func (s *Service) SessionNetwork(ctx context.Context, id string) (SessionNetworkDTO, error) {
	bound, err := s.store.GetSession(ctx, id)
	if err != nil {
		return SessionNetworkDTO{}, err
	}
	out := SessionNetworkDTO{
		SessionID: bound.ID, Mode: normalizedSessionNetworkMode(bound.NetworkMode),
		Fingerprint: bound.NetworkFingerprint, Requested: []string{}, Effective: []string{},
		Alerts: []networkview.Alert{}, Projection: "destinations-withheld",
		Current: SessionNetworkCurrentDTO{Status: "no run yet"},
	}
	if out.Mode != string(egress.Filtered) {
		return out, nil
	}
	policy, err := loadSessionSnapshot(bound)
	if err != nil {
		return SessionNetworkDTO{}, &session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()}
	}
	if policy.ExportDestinations {
		out.Projection = "destinations-included"
		out.Requested, out.Effective = sessionPolicyRuleTexts(policy)
	}
	evidence, err := sessionEvidence(bound)
	if err != nil {
		return SessionNetworkDTO{}, &session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()}
	}
	defer evidence.Close()
	records, _, err := sessionExecutions(evidence, bound.ID)
	if err != nil || len(records) == 0 {
		return out, nil
	}
	newest := records[len(records)-1]
	inspection, err := evidence.Inspect(newest.ID, time.Now(), policy.ExportDestinations)
	if err != nil {
		out.Current = SessionNetworkCurrentDTO{Status: "observation unavailable", RunID: newest.ID}
		return out, nil
	}
	observed := inspection.Observed
	asOf := observed.AsOf
	out.Current = SessionNetworkCurrentDTO{
		Status: inspection.Freshness, RunID: newest.ID, Availability: observed.Availability,
		Scope: observed.Scope, AsOf: &asOf, Counters: observed.Counters, Live: inspection.Current,
		Cleanup: inspection.Cleanup, Sealed: inspection.Receipt != nil,
	}
	if len(observed.Alerts) != 0 {
		out.Alerts = observed.Alerts
	}
	return out, nil
}

// sessionPolicyRuleTexts splits a captured snapshot into what a human asked for and everything
// the box may actually reach. Provider core endpoints and MCP dependencies are derived from the
// authorized selection, so they are effective grants but were never a request.
func sessionPolicyRuleTexts(policy egress.Snapshot) (requested, effective []string) {
	requested, effective = []string{}, []string{}
	for _, grant := range policy.Grants {
		text := box.NetworkRuleText(grant.Rule)
		if text == "" {
			continue
		}
		if !slices.Contains(effective, text) {
			effective = append(effective, text)
		}
		asked := false
		for _, origin := range grant.Origins {
			if origin.Kind == "operator" || origin.Kind == "project" {
				asked = true
			}
		}
		if asked && !slices.Contains(requested, text) {
			requested = append(requested, text)
		}
	}
	slices.Sort(requested)
	slices.Sort(effective)
	return requested, effective
}

// SessionNetworkReceipt aggregates every run this session owned into one versioned receipt. It is
// independent of container garbage collection — the runs are gone, their retained receipts are
// not — and it states finality and completeness separately: a session that is still open can
// never be final, and a final receipt can still be honestly partial.
func (s *Service) SessionNetworkReceipt(ctx context.Context, id string) (SessionNetworkReceiptDTO, error) {
	bound, err := s.store.GetSession(ctx, id)
	if err != nil {
		return SessionNetworkReceiptDTO{}, err
	}
	out := SessionNetworkReceiptDTO{
		SessionID: bound.ID, Mode: normalizedSessionNetworkMode(bound.NetworkMode),
		Fingerprint: bound.NetworkFingerprint,
	}
	if out.Mode != string(egress.Filtered) {
		out.Reason = "this session did not run under restricted networking, so no policy was captured for it"
		return out, nil
	}
	policy, err := loadSessionSnapshot(bound)
	if err != nil {
		return SessionNetworkReceiptDTO{}, &session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()}
	}
	evidence, err := sessionEvidence(bound)
	if err != nil {
		return SessionNetworkReceiptDTO{}, &session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()}
	}
	defer evidence.Close()
	records, complete, err := sessionExecutions(evidence, bound.ID)
	if err != nil {
		return SessionNetworkReceiptDTO{}, &session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()}
	}
	identity := networkview.SessionNetworkIdentity{
		ID: bound.ID, PolicyFingerprint: bound.NetworkFingerprint, AuthorityDigest: bound.AuthorityDigest,
		Mode: egress.Filtered, StartedAt: bound.CreatedAt, RunsComplete: complete,
	}
	if bound.State == session.SessionClosed || bound.State == session.SessionDiscarded {
		closed := bound.UpdatedAt
		identity.ClosedAt = &closed
	}
	receipt, err := networkview.AggregateSessionNetwork(identity,
		sessionRunObservations(evidence, bound, records, policy.ExportDestinations), policy.ExportDestinations)
	if err != nil {
		return SessionNetworkReceiptDTO{}, &session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()}
	}
	out.Available, out.Receipt = true, &receipt
	return out, nil
}

// sessionRunObservations streams this session's runs in the order the aggregator requires — by
// run id, then gateway epoch — stamping each projected receipt with the session authority the
// daemon just verified it against. A run receipt is sealed by the child that produced it and
// knows only its own identity; binding it to the session is the parent's job.
func sessionRunObservations(evidence *networkstate.Evidence, bound session.Session, records []networkstate.Execution, exportDestinations bool) iter.Seq2[networkview.RunObservation, error] {
	ordered := append([]networkstate.Execution(nil), records...)
	slices.SortFunc(ordered, func(a, b networkstate.Execution) int {
		if order := cmp.Compare(a.ID, b.ID); order != 0 {
			return order
		}
		return cmp.Compare(a.Epoch, b.Epoch)
	})
	return func(yield func(networkview.RunObservation, error) bool) {
		for _, record := range ordered {
			inspection, err := evidence.Inspect(record.ID, time.Now(), exportDestinations)
			if err != nil {
				if !yield(networkview.RunObservation{}, err) {
					return
				}
				continue
			}
			observation := inspection.AggregateObservation
			observation.Receipt.AuthorityDigest = bound.AuthorityDigest
			if err := observation.Receipt.SealDigest(); err != nil {
				if !yield(networkview.RunObservation{}, err) {
					return
				}
				continue
			}
			if !yield(observation, nil) {
				return
			}
		}
	}
}

func normalizedSessionNetworkMode(mode string) string {
	if mode == "" {
		return string(egress.Open)
	}
	return mode
}

// SessionNetworkConnectionsDTO is the live drilldown Responder needs: the newest run's bounded
// connection rows exactly as the collector recorded them. It is pull-based on purpose — a
// per-second sample belongs in nobody's durable journal — and it carries its own freshness, so a
// client can tell a stale sample from a closed connection.
type SessionNetworkConnectionsDTO struct {
	SessionID   string                   `json:"session_id"`
	Mode        string                   `json:"mode"`
	RunID       string                   `json:"run_id,omitempty"`
	Status      string                   `json:"status"`
	AsOf        *time.Time               `json:"as_of,omitempty"`
	Connections []networkview.Connection `json:"connections"`
	Truncated   bool                     `json:"detail_truncated,omitempty"`
	Projection  string                   `json:"projection"`
}

// SessionNetworkConnections lists the newest run's observed connections. Like every other network
// read on this API it projects retained evidence: no gateway probe, no runtime, no authority, and
// destinations only when the operator policy opted in.
func (s *Service) SessionNetworkConnections(ctx context.Context, id string) (SessionNetworkConnectionsDTO, error) {
	bound, err := s.store.GetSession(ctx, id)
	if err != nil {
		return SessionNetworkConnectionsDTO{}, err
	}
	out := SessionNetworkConnectionsDTO{
		SessionID: bound.ID, Mode: normalizedSessionNetworkMode(bound.NetworkMode),
		Status: "no run yet", Connections: []networkview.Connection{}, Projection: "destinations-withheld",
	}
	if out.Mode != string(egress.Filtered) {
		out.Status = "not filtered"
		return out, nil
	}
	policy, err := loadSessionSnapshot(bound)
	if err != nil {
		return SessionNetworkConnectionsDTO{}, &session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()}
	}
	if policy.ExportDestinations {
		out.Projection = "destinations-included"
	}
	evidence, err := sessionEvidence(bound)
	if err != nil {
		return SessionNetworkConnectionsDTO{}, &session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()}
	}
	defer evidence.Close()
	records, _, err := sessionExecutions(evidence, bound.ID)
	if err != nil || len(records) == 0 {
		return out, nil
	}
	newest := records[len(records)-1]
	out.RunID = newest.ID
	inspection, err := evidence.Inspect(newest.ID, time.Now(), policy.ExportDestinations)
	if err != nil {
		out.Status = "observation unavailable"
		return out, nil
	}
	asOf := inspection.Observed.AsOf
	out.Status, out.AsOf, out.Truncated = inspection.Freshness, &asOf, inspection.Observed.Loss.DetailTruncated
	if len(inspection.Observed.Connections) != 0 {
		out.Connections = inspection.Observed.Connections
	}
	return out, nil
}

// SessionNetworkExplanationDTO carries one retained refusal and why it happened. A remote client
// without the destination projection still learns the reason and the provenance; the copyable rule
// suggestion stays a local operator view.
type SessionNetworkExplanationDTO struct {
	SessionID   string                         `json:"session_id"`
	Mode        string                         `json:"mode"`
	Available   bool                           `json:"available"`
	Reason      string                         `json:"reason,omitempty"`
	Explanation *networkstate.EventExplanation `json:"explanation,omitempty"`
	Projection  string                         `json:"projection"`
}

// SessionNetworkExplanation opens one retained refusal of this session's own runs. An event that
// aged out of a run's bounded ring reports that honestly — which is not proof it never existed.
func (s *Service) SessionNetworkExplanation(ctx context.Context, id, eventID string) (SessionNetworkExplanationDTO, error) {
	bound, err := s.store.GetSession(ctx, id)
	if err != nil {
		return SessionNetworkExplanationDTO{}, err
	}
	out := SessionNetworkExplanationDTO{
		SessionID: bound.ID, Mode: normalizedSessionNetworkMode(bound.NetworkMode), Projection: "destinations-withheld",
	}
	if out.Mode != string(egress.Filtered) {
		out.Reason = "this session did not run under restricted networking, so it retained no refusals"
		return out, nil
	}
	policy, err := loadSessionSnapshot(bound)
	if err != nil {
		return SessionNetworkExplanationDTO{}, &session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()}
	}
	if policy.ExportDestinations {
		out.Projection = "destinations-included"
	}
	evidence, err := sessionEvidence(bound)
	if err != nil {
		return SessionNetworkExplanationDTO{}, &session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()}
	}
	defer evidence.Close()
	records, _, err := sessionExecutions(evidence, bound.ID)
	if err != nil {
		return SessionNetworkExplanationDTO{}, &session.Error{Code: session.CodeNetworkUnavailable, Detail: err.Error()}
	}
	// Newest run first: an id belongs to exactly one of THIS session's runs, and a client that
	// read it from a network event is most likely looking at the run that just produced it.
	for i := len(records) - 1; i >= 0; i-- {
		explanation, err := evidence.Explain(records[i].ID, eventID, policy.ExportDestinations)
		if err != nil {
			continue
		}
		out.Available, out.Explanation = true, &explanation
		return out, nil
	}
	out.Reason = "event_not_retained"
	return out, nil
}
