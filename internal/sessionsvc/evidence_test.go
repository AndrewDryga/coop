package sessionsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func evidenceCount(value uint64) *networkview.Count { return networkview.Value(value) }

// retainedRunFixture is one sealed filtered run as the collector retains it: a refusal that
// names a destination and drafts a rule, a connection with a peer and a rule id, an alert, a
// source, counters at the 64-bit ceiling and one metric nobody measured.
func retainedRunFixture(export bool) networkstate.Inspection {
	asOf := time.Date(2026, 9, 11, 14, 32, 39, 0, time.UTC)
	started := asOf.Add(-41 * time.Second)
	port := 443
	snapshot := networkview.Snapshot{
		Version: networkview.Version, RunID: "run-7f3a", Epoch: "epoch-1", PolicyFingerprint: strings.Repeat("a", 64),
		Mode: egress.Filtered, Sequence: 42, Terminal: true, AsOf: asOf, ElapsedMillis: 41000,
		Availability: "available", Scope: "proxy-streams-and-sampled-tcp-sockets",
		Health: networkview.HealthLayers{Enforcer: networkview.Health{Status: "ok"}, Gateway: networkview.Health{Status: "ok"},
			Resolver: networkview.Health{Status: "ok"}, Collector: networkview.Health{Status: "degraded", Reason: "socket_sample_lag"}},
		Coverage: networkview.Coverage{ProxyBytes: networkview.MetricCoverage{Status: "exact"}, Connections: networkview.MetricCoverage{Status: "exact"},
			UpstreamFailures: networkview.MetricCoverage{Status: "exact"}, KernelPackets: networkview.MetricCoverage{Status: "lower-bound", Reason: "counter_wrap"},
			GuardDenials: networkview.MetricCoverage{Status: "exact"}, MaintenanceQueries: networkview.MetricCoverage{Status: "exact"},
			MaintenanceBytes: networkview.MetricCoverage{Status: "unavailable", Reason: "not_collected"}, SocketInventory: networkview.MetricCoverage{Status: "sampled"},
			BoundaryAttribution: networkview.MetricCoverage{Status: "exact"}},
		Counters: &networkview.Counters{SentBytes: evidenceCount(184320), ReceivedBytes: evidenceCount(13002342), Connections: evidenceCount(9),
			UpstreamFailures: evidenceCount(0), DeniedPackets: evidenceCount(^uint64(0)), ProtectedPackets: evidenceCount(0),
			DeniedDNSQueries: evidenceCount(0), DeniedTLS: evidenceCount(2), MaintenanceQueries: evidenceCount(3), MaintenanceFailures: evidenceCount(0),
			IngressDenials: evidenceCount(0)},
		Connections: []networkview.Connection{{ID: "conn-0009", DestinationID: "dest-1", State: "closed", Transport: "tcp",
			Name: "api.example.com", NameSource: "sni", Peer: "93.184.216.34:443", RuleID: "rule-1", StartedAt: &started, ObservedAt: asOf,
			SentBytes: evidenceCount(2048), ReceivedBytes: evidenceCount(917504)}},
		Denials: []networkview.Denial{{ID: "evt-0031", Source: "gateway", Sequence: 31, Basis: "sni", DestinationID: "dest-2",
			At: asOf.Add(-19 * time.Second), Kind: "tls_denied", Reason: "no_matching_rule", Name: "blocked.example", Peer: "203.0.113.9:443", Port: &port,
			Candidate: &networkview.Candidate{ID: "cand-1", EvidenceID: "evt-0031", PolicyFingerprint: strings.Repeat("a", 64),
				Rule: egress.Rule{To: egress.Destination{Domain: "blocked.example"}, Protocol: "tls", Ports: []int{443}}, AppliesTo: "project"}}},
		Alerts: []networkview.Alert{{ID: "alert-0002", Sequence: 40, Version: 1, Category: "collector_health", Severity: "warning", State: "raised",
			FirstSeen: asOf.Add(-29 * time.Second), LastSeen: asOf, WindowMillis: 29000, Facts: networkview.AlertFacts{HealthStatus: "degraded", Reason: "socket_sample_lag"}}},
		Loss:    networkview.Loss{Records: 0, Reasons: nil, SuppressedAlerts: 0},
		Sources: []networkview.Source{{ID: "gateway", Sequence: 42, ObservedAt: &asOf, Status: "sealed", Lost: 0}},
	}
	projected := snapshot.Project(export)
	receipt := networkview.Receipt{Version: networkview.Version, ID: "run-7f3a", Snapshot: projected, StartedAt: started,
		Finality: "final", Completeness: "partial", Cleanup: "complete", Runtime: "docker", SessionID: "sess", AttemptID: "attempt-2",
		AuthorityDigest: strings.Repeat("c", 64), CollectorVersion: "gateway-v1"}
	return networkstate.Inspection{Version: networkview.Version, Revision: 3, Observed: projected, Receipt: &receipt,
		AggregateObservation: networkview.RunObservation{Revision: 3, Receipt: receipt},
		Freshness:            networkstate.FreshnessTerminal, ReadAt: asOf.Add(time.Second), Cleanup: "complete"}
}

func filteredReadsFixture(export bool) sessionNetworkReads {
	inspection := retainedRunFixture(export)
	policy := egress.Snapshot{Mode: egress.Filtered, Fingerprint: strings.Repeat("a", 64), ExportDestinations: export}
	if export {
		rule, _ := egress.NormalizeRules([]egress.Rule{{To: egress.Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{443}}})
		policy.Grants = []egress.Grant{{Rule: rule[0], Origins: []egress.Origin{{Kind: "operator", Name: "session-policy"}}}}
	}
	receipt := networkview.SessionReceipt{Version: networkview.Version, SessionID: "sess", PolicyFingerprint: strings.Repeat("a", 64),
		AuthorityDigest: strings.Repeat("c", 64), Mode: egress.Filtered, StartedAt: time.Date(2026, 9, 11, 14, 20, 0, 0, time.UTC),
		Finality: "provisional", Completeness: "partial", Scope: "proxy-streams-and-sampled-tcp-sockets",
		Counters: inspection.Observed.Counters, Coverage: inspection.Observed.Coverage,
		Loss: networkview.Loss{Unknown: true, Reasons: []string{"counter_overflow"}},
		Runs: []networkview.RunReceiptReference{{RunID: "run-7f3a", Epoch: "epoch-1", Sequence: 42, AsOf: inspection.Observed.AsOf,
			Finality: "final", Completeness: "partial", ReceiptDigest: strings.Repeat("e", 64)}},
		RunCount: 1, OmittedReferences: 3, Projection: "destinations-withheld", Digest: strings.Repeat("f", 64), DigestScope: "destinations-withheld"}
	if export {
		receipt.Projection, receipt.DigestScope = "destinations-included", "destinations-included"
	}
	return sessionNetworkReads{policy: policy, records: []networkstate.Execution{{ID: "run-7f3a"}}, complete: true,
		inspection: inspection, receipt: receipt, receiptRead: true}
}

func filteredSessionFixture() session.Session {
	return session.Session{ID: "sess", NetworkMode: string(egress.Filtered), NetworkFingerprint: strings.Repeat("a", 64),
		NetworkQualification: strings.Repeat("b", 64), AuthorityDigest: strings.Repeat("c", 64), Revision: 4, State: session.SessionOpen,
		CreatedAt: time.Date(2026, 9, 11, 14, 20, 0, 0, time.UTC)}
}

// The projection is an allowlist over records the daemon already projected. With export off a
// refusal keeps its reason, basis and port but no name, peer, rule or drafted candidate; with
// export on the name appears and nothing else changes. Counters cross as decimal strings, an
// unmeasured metric stays null, and a coverage word this contract does not know is unavailable.
func TestSessionEvidenceProjectsARetainedRunWithoutDisclosingDestinations(t *testing.T) {
	bound := filteredSessionFixture()
	withheld := networkEvidenceFromReads(bound, filteredReadsFixture(false))
	if err := (workerproto.SessionEvidence{Version: 1, CapturedAt: time.Now(), SessionID: "sess", Revision: 4, State: "open",
		Network: withheld, Task: workerproto.TaskEvidence{Status: workerproto.EvidenceStatusUnbound}}).Validate(); err != nil {
		t.Fatalf("withheld evidence failed its own contract: %v", err)
	}
	if withheld.Access.Status != "captured" || *withheld.Access.Projection != "destinations-withheld" ||
		len(withheld.Access.Requested) != 0 || *withheld.Access.Qualification != strings.Repeat("b", 64) {
		t.Fatalf("withheld access = %+v", withheld.Access)
	}
	observation := withheld.Observation
	if observation.Status != "observed" || *observation.Freshness != "terminal" || *observation.RunID != "run-7f3a" ||
		*observation.AttemptID != "attempt-2" || *observation.Sequence != "42" || !observation.Sealed || *observation.CleanupOutcome != "complete" {
		t.Fatalf("observation identity = %+v", observation)
	}
	if got := *observation.Counters.DeniedPackets; got != "18446744073709551615" {
		t.Fatalf("denied packets = %s, want the exact 64-bit value", got)
	}
	if observation.Counters.MaintenanceSentBytes != nil {
		t.Fatalf("an unmeasured counter became %q", *observation.Counters.MaintenanceSentBytes)
	}
	if observation.Coverage.SocketInventory.Status != "unavailable" || *observation.Coverage.SocketInventory.Reason != "sampled" ||
		observation.Coverage.KernelPackets.Status != "lower-bound" {
		t.Fatalf("coverage = %+v", *observation.Coverage)
	}
	if observation.Health.Collector.Status != "degraded" || *observation.Health.Collector.Reason != "socket_sample_lag" {
		t.Fatalf("health = %+v", *observation.Health)
	}
	denial := observation.Denials[0]
	if denial.Destination != nil || !denial.DestinationWithheld || denial.Basis != "tls" || denial.Reason != "no_matching_rule" ||
		*denial.Port != 443 || denial.SourceSequence != "31" {
		t.Fatalf("withheld denial = %+v", denial)
	}
	connection := observation.Connections[0]
	if connection.Destination != nil || !connection.DestinationWithheld || connection.RuleID != nil || *connection.ReceivedBytes != "917504" {
		t.Fatalf("withheld connection = %+v", connection)
	}
	if observation.Alerts[0].Category != "collector_health" || *observation.Alerts[0].HealthStatus != "degraded" ||
		observation.Sources[0].Status != "sealed" || observation.Sources[0].Sequence != "42" {
		t.Fatalf("alerts/sources = %+v / %+v", observation.Alerts, observation.Sources)
	}
	encoded, _ := json.Marshal(withheld)
	for _, secret := range []string{"blocked.example", "203.0.113.9", "api.example.com", "93.184.216.34", "rule-1", "cand-1", "example.com"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("withheld projection leaked %q: %s", secret, encoded)
		}
	}
	receipt := withheld.Receipt
	if receipt.Status != "available" || *receipt.Finality != "provisional" || *receipt.Completeness != "partial" ||
		*receipt.RunCount != "1" || *receipt.OmittedRunReferences != "3" || !receipt.Loss.Unknown || receipt.Loss.Reasons[0] != "counter_overflow" ||
		len(receipt.Runs) != 1 || receipt.Runs[0].ReceiptDigest != strings.Repeat("e", 64) || *receipt.ReceiptDigest != strings.Repeat("f", 64) {
		t.Fatalf("receipt = %+v", receipt)
	}

	// Defence in depth: the daemon's own Project already withheld those names, so this asserts
	// the EXPORT withholds too. Feed it a snapshot that still carries every name and peer while
	// the policy says withheld — the shape a future refactor that forgets one Project call
	// produces — and nothing may cross.
	unprojected := filteredReadsFixture(false)
	unprojected.inspection.Observed = retainedRunFixture(true).Observed
	leaky := networkEvidenceFromReads(bound, unprojected)
	if leaky.Observation.Denials[0].Destination != nil || !leaky.Observation.Denials[0].DestinationWithheld ||
		leaky.Observation.Connections[0].Destination != nil || leaky.Observation.Connections[0].RuleID != nil {
		t.Fatalf("the export relied on an upstream projection: %+v / %+v",
			leaky.Observation.Denials[0], leaky.Observation.Connections[0])
	}

	included := networkEvidenceFromReads(bound, filteredReadsFixture(true))
	if *included.Access.Projection != "destinations-included" || len(included.Access.Requested) != 1 ||
		included.Access.Requested[0] != "example.com tls/443" || len(included.Access.Effective) != 1 {
		t.Fatalf("included access = %+v", included.Access)
	}
	denial = included.Observation.Denials[0]
	if denial.Destination == nil || *denial.Destination != "blocked.example" || denial.DestinationWithheld {
		t.Fatalf("included denial = %+v", denial)
	}
	connection = included.Observation.Connections[0]
	if connection.Destination == nil || *connection.Destination != "api.example.com" || *connection.RuleID != "rule-1" {
		t.Fatalf("included connection = %+v", connection)
	}
	if *included.Receipt.Projection != "destinations-included" || *included.Receipt.DigestScope != "destinations-included" {
		t.Fatalf("included receipt projection = %+v", included.Receipt)
	}
}

// Lists are bounded on the way out and the drop is counted, so a run with a thousand refusals
// costs one bounded object that still says how much it left behind.
func TestSessionEvidenceBoundsListsAndCountsWhatItDropped(t *testing.T) {
	reads := filteredReadsFixture(false)
	observed := reads.inspection.Observed
	for i := 0; i < workerproto.MaxSessionEvidenceDenials+5; i++ {
		observed.Denials = append(observed.Denials, observed.Denials[0])
	}
	for i := 0; i < workerproto.MaxSessionEvidenceAlerts+2; i++ {
		observed.Alerts = append(observed.Alerts, observed.Alerts[0])
	}
	reads.inspection.Observed = observed
	for i := 0; i < workerproto.MaxSessionEvidenceRunRefs+7; i++ {
		reads.receipt.Runs = append(reads.receipt.Runs, reads.receipt.Runs[0])
	}
	out := networkEvidenceFromReads(filteredSessionFixture(), reads)
	if len(out.Observation.Denials) != workerproto.MaxSessionEvidenceDenials || out.Observation.OmittedDenials != 6 ||
		len(out.Observation.Alerts) != workerproto.MaxSessionEvidenceAlerts || out.Observation.OmittedAlerts != 3 ||
		out.Observation.OmittedConnections != 0 {
		t.Fatalf("bounded observation = %d denials (%d omitted), %d alerts (%d omitted)", len(out.Observation.Denials),
			out.Observation.OmittedDenials, len(out.Observation.Alerts), out.Observation.OmittedAlerts)
	}
	if len(out.Receipt.Runs) != workerproto.MaxSessionEvidenceRunRefs || *out.Receipt.OmittedRunReferences != "11" {
		t.Fatalf("bounded receipt = %d runs, %s omitted (daemon omitted 3 + export dropped 8)", len(out.Receipt.Runs), *out.Receipt.OmittedRunReferences)
	}
}

// Each network section fails on its own. A registry that cannot answer at all leaves the posture
// known and every section unavailable with the cause; one that lists no run is a different fact
// from one that lists a run it cannot read.
func TestSessionEvidenceReportsEachUnreadableNetworkSectionSeparately(t *testing.T) {
	bound := filteredSessionFixture()
	unreadable := networkEvidenceFromReads(bound, sessionNetworkReads{policyErr: errors.New("owner key missing")})
	if unreadable.Mode != "filtered" || *unreadable.Fingerprint != strings.Repeat("a", 64) {
		t.Fatalf("posture forgotten: %+v", unreadable)
	}
	for name, status := range map[string]string{"access": unreadable.Access.Status, "observation": unreadable.Observation.Status, "receipt": unreadable.Receipt.Status} {
		if status != "unavailable" {
			t.Fatalf("%s = %s, want unavailable", name, status)
		}
	}
	if !strings.Contains(*unreadable.Receipt.Reason, "owner key missing") {
		t.Fatalf("receipt reason = %q", *unreadable.Receipt.Reason)
	}

	reads := filteredReadsFixture(false)
	reads.records, reads.recordsErr, reads.receiptRead = nil, errors.New("inventory page torn"), false
	partial := networkEvidenceFromReads(bound, reads)
	if partial.Access.Status != "captured" || partial.Observation.Status != "unavailable" || partial.Receipt.Status != "unavailable" ||
		!strings.Contains(*partial.Observation.Reason, "inventory page torn") {
		t.Fatalf("torn inventory = %+v", partial)
	}

	reads = filteredReadsFixture(false)
	reads.records = nil
	quiet := networkEvidenceFromReads(bound, reads)
	if quiet.Observation.Status != "no_run" || quiet.Observation.Reason != nil || quiet.Receipt.Status != "available" {
		t.Fatalf("no run = %+v", quiet)
	}

	reads = filteredReadsFixture(false)
	reads.inspectErr = errors.New("snapshot unreadable")
	stale := networkEvidenceFromReads(bound, reads)
	if stale.Observation.Status != "unavailable" || stale.Receipt.Status != "available" {
		t.Fatalf("unreadable newest run = %+v", stale)
	}
	for _, evidence := range []workerproto.NetworkEvidence{unreadable, partial, quiet, stale} {
		if err := (workerproto.SessionEvidence{Version: 1, CapturedAt: time.Now(), SessionID: "sess", Revision: 4, State: "open",
			Network: evidence, Task: workerproto.TaskEvidence{Status: workerproto.EvidenceStatusUnbound}}).Validate(); err != nil {
			t.Fatalf("degraded evidence failed its own contract: %v", err)
		}
	}
}

// The route answers the honest shape for a session that never ran filtered and never bound a
// task, and the daemon proves the read through its capabilities so a worker can advertise it.
func TestSessionEvidenceRouteForAnOpenSessionWithoutATask(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "open-evidence", CreateRemoteSessionRequest{
		Policy: "responder", Task: "open session",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(service)
	response := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/evidence", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("evidence status=%d body=%s", response.Code, response.Body.String())
	}
	evidence, err := workerproto.DecodeSessionEvidence(response.Body.Bytes())
	if err != nil {
		t.Fatalf("the route published an object outside its contract: %v\n%s", err, response.Body.String())
	}
	if evidence.SessionID != sess.ID || evidence.Revision != sess.Revision || evidence.State != "open" ||
		evidence.Network.Mode != "open" || evidence.Network.Access.Status != "not_filtered" ||
		evidence.Network.Observation.Status != "not_filtered" || evidence.Network.Receipt.Status != "not_filtered" ||
		evidence.Task.Status != "unbound" {
		t.Fatalf("open evidence = %+v", evidence)
	}
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		response = sessionHTTPTestRequest(t, handler, method, "/v1/sessions/"+sess.ID+"/evidence", "{}", "mutate", "application/json")
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s evidence status=%d", method, response.Code)
		}
	}
	response = sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/evidence?destinations=1", "", "", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a query on the evidence read was accepted: %d", response.Code)
	}
	response = sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/nope/evidence", "", "", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown session evidence status=%d", response.Code)
	}
	response = sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/capabilities", "", "", "")
	var capabilities map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	versions, ok := capabilities["session_evidence_versions"].([]any)
	if !ok || len(versions) != 1 || versions[0] != float64(workerproto.SessionEvidenceVersion) {
		t.Fatalf("capabilities session evidence versions = %#v", capabilities["session_evidence_versions"])
	}
}

// A filtered session that has not run yet exports its captured posture and an explicit no-run
// observation — the receipt exists, provisional, with nothing observed — never an empty counter.
func TestSessionEvidenceRouteForAFilteredSessionBeforeAnyRun(t *testing.T) {
	for _, export := range []bool{false, true} {
		service, repo := newHTTPTestSessionService(t)
		fingerprint := admitTestNetworkSnapshot(t, repo, export)
		service.testAdmitNetwork = func(Policy, string, string) (sessionNetworkBinding, error) {
			return sessionNetworkBinding{Mode: egress.Filtered, Fingerprint: fingerprint, Qualification: strings.Repeat("b", 64)}, nil
		}
		sess, err := service.CreateRemoteSession(context.Background(), "filtered-evidence", CreateRemoteSessionRequest{
			Policy: "responder", Task: "filtered session",
		})
		if err != nil {
			t.Fatal(err)
		}
		response := sessionHTTPTestRequest(t, NewHTTPHandler(service), http.MethodGet, "/v1/sessions/"+sess.ID+"/evidence", "", "", "")
		if response.Code != http.StatusOK {
			t.Fatalf("evidence status=%d body=%s", response.Code, response.Body.String())
		}
		evidence, err := workerproto.DecodeSessionEvidence(response.Body.Bytes())
		if err != nil {
			t.Fatalf("export=%v: %v\n%s", export, err, response.Body.String())
		}
		network := evidence.Network
		if network.Mode != "filtered" || *network.Fingerprint != fingerprint || network.Access.Status != "captured" ||
			*network.Access.Qualification != strings.Repeat("b", 64) || network.Observation.Status != "no_run" ||
			network.Receipt.Status != "available" || *network.Receipt.Finality != "provisional" || *network.Receipt.RunCount != "0" ||
			*network.Receipt.Scope != "not-observed" || network.Receipt.Counters != nil {
			t.Fatalf("export=%v evidence = %+v", export, network)
		}
		if export {
			if *network.Access.Projection != "destinations-included" || !contains(network.Access.Requested, "example.com tls/443") {
				t.Fatalf("exported access = %+v", network.Access)
			}
		} else if *network.Access.Projection != "destinations-withheld" || len(network.Access.Requested) != 0 || len(network.Access.Effective) != 0 {
			t.Fatalf("withheld access = %+v", network.Access)
		}
		service.Stop()
	}
}

// The task snapshot reads the same projection a checkpoint captures, so its digest is the
// checkpoint's; the checklist carries the labels the checkpoint's booleans stand for; the state
// note is bounded and withheld whole when it looks like a secret; a task folder that has gone
// missing reports unavailable with its identity intact rather than an empty task.
func TestSessionEvidenceSnapshotsTheBoundTaskWithTheCheckpointDigest(t *testing.T) {
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".agent/tasks/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitignore")
	git("commit", "-qm", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), nil)
	defer service.Stop()
	ctx := context.Background()
	sess, err := service.CreateRemoteSession(ctx, "evidence-task", CreateRemoteSessionRequest{Policy: "responder", Task: "record:task_offer:evidence"})
	if err != nil {
		t.Fatal(err)
	}
	draft := tasks.ControllerTaskDraft{
		OfferRef: "record:task_offer:evidence", Title: "Fix API timeout", Prompt: "Raise the deadline.",
		SuccessChecks: []string{"Reproduce the timeout", "Run the owning tests"},
	}
	sess, err = service.EnsureWorkspaceTask(ctx, "evidence-ensure", EnsureWorkspaceTaskRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Task: draft})
	if err != nil {
		t.Fatal(err)
	}
	queue := filepath.Join(sess.Workspace, tasks.TasksRoot)
	item, ok := mustCurrentTask(t, queue, sess.WorkspaceTask.ID)
	if !ok {
		t.Fatal("workspace task disappeared")
	}
	body, err := os.ReadFile(filepath.Join(item.Dir, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item.Dir, "task.md"), bytes.Replace(body, []byte("- [ ]"), []byte("- [x]"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item.Dir, "state.md"), []byte("# State — Fix API timeout\n\n**Status:** deadline raised\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	item, _ = mustCurrentTask(t, queue, item.ID)
	if err := tasks.MoveTaskDir(queue, item, tasks.StateInProgress); err != nil {
		t.Fatal(err)
	}

	evidence, err := service.SessionEvidence(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := evidence.Task
	if task.Status != "bound" || *task.OfferRef != draft.OfferRef || *task.QueueID != sess.WorkspaceTask.QueueID ||
		*task.TaskID != sess.WorkspaceTask.TaskID || *task.ID != sess.WorkspaceTask.ID || *task.DraftSHA256 != sess.WorkspaceTask.DraftSHA256 {
		t.Fatalf("task binding = %+v", task)
	}
	snapshot := task.Snapshot
	if snapshot.State != "in_progress" || snapshot.Title != "Fix API timeout" || len(snapshot.Checklist) != 2 ||
		snapshot.Checklist[0] != (workerproto.TaskChecklistItem{Label: "Reproduce the timeout", Checked: true}) ||
		snapshot.Checklist[1] != (workerproto.TaskChecklistItem{Label: "Run the owning tests", Checked: false}) ||
		snapshot.HasDecision {
		t.Fatalf("task snapshot = %+v", *snapshot)
	}
	if snapshot.StateNote.Status != "captured" || !strings.Contains(*snapshot.StateNote.Text, "deadline raised") || snapshot.StateNote.Truncated {
		t.Fatalf("state note = %+v", snapshot.StateNote)
	}
	names := map[string]bool{}
	for _, file := range snapshot.Files {
		names[file.Path] = true
		if file.ByteSize < 0 || len(file.SHA256) != 64 || strings.HasPrefix(file.Path, "/") {
			t.Fatalf("task file = %+v", file)
		}
	}
	taskDir := "10_in_progress/" + item.ID + "/"
	if !names[".coop-queue.json"] || !names[taskDir+"task.md"] || !names[taskDir+"state.md"] || !names[taskDir+"log.md"] {
		t.Fatalf("task files = %v", names)
	}

	result, err := service.CheckpointWorkspace(ctx, "evidence-checkpoint", CheckpointWorkspaceRequest{
		SessionID: sess.ID, SessionRef: "session-work-1", ExpectedRevision: sess.Revision, PlacementGeneration: 1, RepositoryRef: "responder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Checkpoint.Task.StateSHA256 != snapshot.StateSHA256 || len(result.Checkpoint.Task.Subtasks) != 2 || !result.Checkpoint.Task.Subtasks[0] {
		t.Fatalf("checkpoint task %+v does not agree with the snapshot digest %s", result.Checkpoint.Task, snapshot.StateSHA256)
	}

	secret := "# State\n\nexport GITHUB_TOKEN=ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij123456\n"
	if err := os.WriteFile(filepath.Join(queue, taskDir, "state.md"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	evidence, err = service.SessionEvidence(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	note := evidence.Task.Snapshot.StateNote
	if note.Status != "withheld" || note.Text != nil || note.Reason == nil {
		t.Fatalf("a secret-bearing note was exported: %+v", note)
	}
	if encoded, _ := json.Marshal(evidence); bytes.Contains(encoded, []byte("ghp_")) {
		t.Fatalf("the secret leaked through another field: %s", encoded)
	}
	if evidence.Task.Snapshot.StateSHA256 == snapshot.StateSHA256 {
		t.Fatal("a changed task folder kept the old state digest")
	}

	long := strings.Repeat("progress line\n", 1000)
	if err := os.WriteFile(filepath.Join(queue, taskDir, "state.md"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	evidence, err = service.SessionEvidence(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	note = evidence.Task.Snapshot.StateNote
	if note.Status != "captured" || !note.Truncated || len(*note.Text) != workerproto.MaxSessionEvidenceNoteBytes {
		t.Fatalf("long note = status %s truncated %v len %d", note.Status, note.Truncated, len(*note.Text))
	}

	if err := os.RemoveAll(filepath.Join(queue, taskDir)); err != nil {
		t.Fatal(err)
	}
	evidence, err = service.SessionEvidence(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Task.Status != "unavailable" || evidence.Task.Snapshot != nil || evidence.Task.Reason == nil ||
		evidence.Task.OfferRef == nil || *evidence.Task.OfferRef != draft.OfferRef {
		t.Fatalf("missing task folder = %+v", evidence.Task)
	}
	if err := evidence.Validate(); err != nil {
		t.Fatalf("unavailable task evidence failed its contract: %v", err)
	}
}
