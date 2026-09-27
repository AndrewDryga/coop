package sessionsvc

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/session"
)

// Creation freezes the posture on the immutable row. Every later run reads it from there, so if
// it were not persisted the session would silently fall back to open on the next turn.
func TestCreateFreezesTheSessionNetworkBinding(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	service.testAdmitNetwork = func(_, _ string, _ executionConfig, workspace, forkName string) (sessionNetworkBinding, error) {
		if workspace == "" || forkName == "" {
			t.Errorf("admission ran without a workspace (%q) or fork (%q)", workspace, forkName)
		}
		return sessionNetworkBinding{
			Mode: egress.Filtered, Fingerprint: strings.Repeat("a", 64), Qualification: strings.Repeat("b", 64),
		}, nil
	}
	ctx := context.Background()
	sess, err := service.CreateRemoteSession(ctx, "create-network", service.request(t, "freeze-the-posture"))
	if err != nil {
		t.Fatal(err)
	}
	if sess.NetworkMode != string(egress.Filtered) || sess.NetworkFingerprint != strings.Repeat("a", 64) ||
		sess.NetworkQualification != strings.Repeat("b", 64) {
		t.Fatalf("created session network = %q/%q/%q", sess.NetworkMode, sess.NetworkFingerprint, sess.NetworkQualification)
	}
	reread, err := service.GetSession(ctx, sess.ID)
	if err != nil || reread.NetworkFingerprint != sess.NetworkFingerprint {
		t.Fatalf("reread session network = %+v, err %v", reread.NetworkFingerprint, err)
	}
	handler := NewHTTPHandler(service.Service)
	response := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID, "", "", "")
	var dto struct {
		Network SessionNetworkSummaryDTO `json:"network"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Network.Mode != "filtered" || dto.Network.Fingerprint != strings.Repeat("a", 64) {
		t.Fatalf("session DTO network = %+v", dto.Network)
	}
}

// A session whose network authority cannot be resolved must not be created open instead. The
// refusal carries the host's own reason, because only an operator can act on it.
func TestCreateRefusesWhenNetworkAdmissionFails(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	service.testAdmitNetwork = func(string, string, executionConfig, string, string) (sessionNetworkBinding, error) {
		return sessionNetworkBinding{}, errNetworkFixture
	}
	_, err := service.CreateRemoteSession(context.Background(), "create-network-refused", service.request(t, "refuse-the-posture"))
	if err == nil || session.CodeOf(err) != session.CodeNetworkUnavailable ||
		!strings.Contains(err.Error(), "this host is not set up for filtered runs") {
		t.Fatalf("create error = %v (code %q)", err, session.CodeOf(err))
	}
}

// An offline session's box reaches no server by URL, so its private MCP copy carries only local
// servers — a bound Responder endpoint among the ones left out. The binding is refused by name,
// at create and at a turn on a session created before that refusal existed, instead of being
// accepted and then silently dropped while the receipt still claims one is bound.
func TestAnOfflineSessionRefusesAControllerTools(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	offline := service.Job
	offline.Egress.Mode = "none"
	service.testAdmitNetwork = func(_, _ string, policy executionConfig, _, _ string) (sessionNetworkBinding, error) {
		return sessionNetworkBinding{Mode: policy.Egress.Mode, Fingerprint: strings.Repeat("a", 64),
			Qualification: strings.Repeat("b", 64)}, nil
	}
	binding := &session.ControllerTools{Endpoint: "https://responder.example/v1/state-tools/mcp", Token: strings.Repeat("b", 48)}
	ctx := context.Background()
	if _, err := service.CreateRemoteSession(ctx, "create-offline-bound", func() CreateRemoteSessionRequest {
		request := jobRequest(t, offline, "test:offline")
		request.ControllerTools = binding
		return request
	}()); err == nil || !strings.Contains(err.Error(), "binds no controller MCP endpoint") {
		t.Fatalf("offline create with a Responder binding = %v", err)
	}
	// The same binding on an online session is still accepted: the refusal is about the posture.
	if _, err := service.CreateRemoteSession(ctx, "create-open-bound", func() CreateRemoteSessionRequest {
		request := service.request(t, "test:online")
		request.ControllerTools = binding
		return request
	}()); err != nil {
		t.Fatalf("open create with a Responder binding = %v", err)
	}
	// A turn binding on an offline session — the shape a session created by an earlier binary can
	// still carry — is refused the same way.
	sess, err := service.CreateRemoteSession(ctx, "create-offline", jobRequest(t, offline, "test:offline-work"))
	if err != nil {
		t.Fatal(err)
	}
	if sess.NetworkMode != string(egress.None) {
		t.Fatalf("offline session network mode = %q", sess.NetworkMode)
	}
	err = service.validateTurnEscalation(ctx, session.SubmitTurnRequest{SessionID: sess.ID, Prompt: "x", ControllerTools: binding})
	if err == nil || !strings.Contains(err.Error(), "binds no controller MCP endpoint") {
		t.Fatalf("offline turn with a Responder binding = %v", err)
	}
	if err := service.validateTurnEscalation(ctx, session.SubmitTurnRequest{SessionID: sess.ID, Prompt: "x"}); err != nil {
		t.Fatalf("an offline turn without a binding = %v", err)
	}
}

var errNetworkFixture = &session.Error{
	Code: session.CodeInvalidRequest, Detail: "this host is not set up for filtered runs with this Docker and these agents",
}

// The child receives only explicit frozen job authority, never ambient host defaults.
func TestNetworkChildEnvironmentCarriesOnlyAFrozenPosture(t *testing.T) {
	open := session.Session{ID: "remote_1", Repository: "/srv/app", NetworkMode: "open",
		JobDigest: strings.Repeat("c", 64), JobDocument: []byte(`{"version":1}`)}
	if env, err := networkChildEnvironment(open, "session-abc"); err != nil || len(env) != 2 || env[0] != "COOP_CONTROLLER_JOB="+open.JobDigest || env[1] != "COOP_EGRESS=open" {
		t.Fatalf("open session env = %v, err %v", env, err)
	}
	offline := open
	offline.NetworkMode = "none"
	env, err := networkChildEnvironment(offline, "session-abc")
	if err != nil || len(env) != 2 || env[1] != "COOP_EGRESS=none" {
		t.Fatalf("offline session env = %v, err %v", env, err)
	}
	filtered := session.Session{
		ID: "remote_1", Repository: "/srv/app", NetworkMode: "filtered",
		NetworkFingerprint: strings.Repeat("a", 64), NetworkQualification: strings.Repeat("b", 64),
		JobDigest: open.JobDigest, JobDocument: open.JobDocument,
	}
	env, err = networkChildEnvironment(filtered, "session-abc")
	if err != nil || len(env) != 3 || env[1] != "COOP_EGRESS=filtered" {
		t.Fatalf("filtered session env = %v, err %v", env, err)
	}
	value, ok := strings.CutPrefix(env[2], "COOP_NETWORK_CAPTURE=")
	if !ok {
		t.Fatalf("filtered session env = %v", env)
	}
	var capture struct {
		Project       string `json:"project"`
		Fingerprint   string `json:"fingerprint"`
		Qualification string `json:"qualification"`
		SessionID     string `json:"session_id"`
		AttemptID     string `json:"attempt_id"`
		JobDigest     string `json:"job_digest"`
	}
	if err := json.Unmarshal([]byte(value), &capture); err != nil {
		t.Fatal(err)
	}
	if capture.Project != "/srv/app" || capture.Fingerprint != filtered.NetworkFingerprint ||
		capture.Qualification != filtered.NetworkQualification ||
		capture.SessionID != "remote_1" || capture.AttemptID != "session-abc" || capture.JobDigest != open.JobDigest {
		t.Fatalf("capture = %+v", capture)
	}
	// A filtered session with no captured policy is a broken row, not an open launch.
	broken := filtered
	broken.NetworkFingerprint = ""
	if _, err := networkChildEnvironment(broken, "session-abc"); err == nil {
		t.Fatal("a filtered session with no fingerprint produced a launch environment")
	}
	for _, broken := range []session.Session{
		{NetworkMode: "open"},
		{JobDigest: open.JobDigest, JobDocument: open.JobDocument},
		{JobDigest: open.JobDigest, NetworkMode: "open"},
	} {
		if _, err := networkChildEnvironment(broken, "session-abc"); err == nil {
			t.Fatal("incomplete or legacy authority produced a launch environment")
		}
	}
}

// The daemon writes the fingerprint into the immutable session row; the box records what it
// actually enforced. A run that enforced anything else is a custody failure — the turn fails.
func TestSessionNetworkRunsMustMatchTheSessionFingerprint(t *testing.T) {
	bound := session.Session{ID: "remote_1", NetworkMode: "filtered", NetworkFingerprint: strings.Repeat("a", 64)}
	matching := networkExecutionFixture("run-1", strings.Repeat("a", 64))
	if run, err := verifySessionNetworkRuns(bound, []networkstate.Execution{matching}); err != nil || run != "" {
		t.Fatalf("matching run rejected: %q, %v", run, err)
	}
	other := networkExecutionFixture("run-2", strings.Repeat("f", 64))
	run, err := verifySessionNetworkRuns(bound, []networkstate.Execution{matching, other})
	if err == nil || run != "run-2" || !strings.Contains(err.Error(), "run-2") || !strings.Contains(err.Error(), bound.ID) {
		t.Fatalf("mismatch = %q, %v", run, err)
	}
}

func networkExecutionFixture(id, fingerprint string) networkstate.Execution {
	record := networkstate.Execution{ID: id}
	record.Snapshot.PolicyFingerprint = fingerprint
	return record
}

// The two read routes answer honestly for a session that never ran filtered: there is no capture,
// so there is no receipt — which is a different fact from a filtered session that has not run.
func TestSessionNetworkRoutesForAnOpenSession(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "open-network", service.request(t, "open-session"))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(service.Service)
	response := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/network", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("network status=%d body=%s", response.Code, response.Body.String())
	}
	var view SessionNetworkDTO
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Mode != "open" || view.Current.Status != "no run yet" || view.Projection != "destinations-withheld" {
		t.Fatalf("open session network = %+v", view)
	}
	response = sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/network/receipt", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("receipt status=%d body=%s", response.Code, response.Body.String())
	}
	var receipt SessionNetworkReceiptDTO
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Available || receipt.Receipt != nil || receipt.Reason == "" {
		t.Fatalf("open session receipt = %+v", receipt)
	}
	// Reads only: a mutation on either path is not a route that exists.
	for _, path := range []string{"/network", "/network/receipt"} {
		response = sessionHTTPTestRequest(t, handler, http.MethodPost,
			"/v1/sessions/"+sess.ID+path, "{}", "mutate", "application/json")
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status=%d", path, response.Code)
		}
	}
}

// The destination projection is the policy's decision, not the caller's. With export off the
// routes answer with counts, health and reasons; with it on they name the rules.
func TestSessionNetworkRoutesProjectDestinationsByPolicy(t *testing.T) {
	for _, export := range []bool{false, true} {
		name := "withheld"
		if export {
			name = "exported"
		}
		t.Run(name, func(t *testing.T) {
			service, _ := newHTTPTestSessionService(t)
			defer service.Stop()
			service.Job.Egress.Mode, service.Job.Egress.ExportDestinations = "filtered", export
			var fingerprint string
			service.testAdmitNetwork = func(jobDigest, sessionID string, _ executionConfig, _, _ string) (sessionNetworkBinding, error) {
				fingerprint = admitTestNetworkSnapshot(t, jobDigest, sessionID, export)
				return sessionNetworkBinding{
					Mode: egress.Filtered, Fingerprint: fingerprint, Qualification: strings.Repeat("b", 64),
				}, nil
			}
			sess, err := service.CreateRemoteSession(context.Background(), "filtered-network", service.request(t, "filtered-session"))
			if err != nil {
				t.Fatal(err)
			}
			handler := NewHTTPHandler(service.Service)
			response := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/network", "", "", "")
			if response.Code != http.StatusOK {
				t.Fatalf("network status=%d body=%s", response.Code, response.Body.String())
			}
			var view SessionNetworkDTO
			if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.Mode != "filtered" || view.Fingerprint != fingerprint {
				t.Fatalf("filtered network view = %+v", view)
			}
			if export {
				if view.Projection != "destinations-included" ||
					!contains(view.Requested, "example.com tls/443") || !contains(view.Effective, "example.com tls/443") {
					t.Fatalf("exported view = %+v", view)
				}
			} else if view.Projection != "destinations-withheld" || len(view.Requested) != 0 || len(view.Effective) != 0 {
				t.Fatalf("withheld view = %+v", view)
			}
			response = sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/network/receipt", "", "", "")
			if response.Code != http.StatusOK {
				t.Fatalf("receipt status=%d body=%s", response.Code, response.Body.String())
			}
			var receipt SessionNetworkReceiptDTO
			if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
				t.Fatal(err)
			}
			if !receipt.Available || receipt.Receipt == nil {
				t.Fatalf("filtered receipt = %+v", receipt)
			}
			// An open session can never be final, and a session with no run has observed nothing.
			if receipt.Receipt.Finality != "provisional" || receipt.Receipt.RunCount != 0 ||
				receipt.Receipt.Scope != "not-observed" {
				t.Fatalf("receipt = %+v", *receipt.Receipt)
			}
			wantProjection := "destinations-withheld"
			if export {
				wantProjection = "destinations-included"
			}
			if receipt.Receipt.Projection != wantProjection {
				t.Fatalf("receipt projection = %q, want %q", receipt.Receipt.Projection, wantProjection)
			}
		})
	}
}

// The `network` event is how a refusal reaches a client that is not watching a terminal, so it
// has to arrive on the same durable stream every other session fact does — payload included.
func TestSessionNetworkEventReachesTheEventStream(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	ctx := context.Background()
	sess, err := service.CreateRemoteSession(ctx, "network-event", service.request(t, "network-event"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := sessionNetworkPayload(boxNetworkReportFixture())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store().AppendEvent(ctx, session.AppendEventRequest{
		SessionID: sess.ID, Type: session.EventNetwork, Version: sessionNetworkEventVersion, Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(service.Service)
	response := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/events", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", response.Code, response.Body.String())
	}
	var page []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range page {
		if event.Type != string(session.EventNetwork) {
			continue
		}
		found = true
		var decoded sessionNetworkEventPayload
		if err := json.Unmarshal(event.Payload, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.RunID != "run-1" || len(decoded.Denials) != 1 ||
			decoded.Denials[0].Destination != "blocked.example" || decoded.Denials[0].Count != 3 {
			t.Fatalf("network event payload = %+v", decoded)
		}
	}
	if !found {
		t.Fatalf("no network event on the stream: %s", response.Body.String())
	}
}

// admitTestNetworkSnapshot freezes a real owner-keyed snapshot for the session's repository, so
// the read routes authenticate against the same store a launch would.
func admitTestNetworkSnapshot(t *testing.T, jobDigest, sessionID string, export bool) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := filepath.Join(os.Getenv("XDG_STATE_HOME"), "coop", "network")
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rules, err := egress.NormalizeRules([]egress.Rule{
		{To: egress.Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{443}},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := store.CaptureJob(networkstate.JobSnapshotRef{JobDigest: jobDigest, SessionID: sessionID}, rules, nil, export)
	if err != nil {
		t.Fatal(err)
	}
	return policy.Fingerprint
}

// boxNetworkReportFixture is one refused destination as the run summary groups it.
func boxNetworkReportFixture() box.NetworkReport {
	return box.NetworkReport{
		RunID: "run-1", Allowed: "allowed: 1 connection, 10 B sent, 20 B received",
		Denials: []box.NetworkDenial{{Destination: "blocked.example", Basis: "dns", Count: 3}},
		Event:   "n1",
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// A run that enforced a policy this session was never granted is a custody failure, not a
// reporting detail: the turn fails, and the stream says which run and which policy.
func TestSessionNetworkMismatchFailsTheTurnAndReachesTheStream(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	fingerprint := strings.Repeat("a", 64)
	service.testAdmitNetwork = func(string, string, executionConfig, string, string) (sessionNetworkBinding, error) {
		return sessionNetworkBinding{
			Mode: egress.Filtered, Fingerprint: fingerprint, Qualification: strings.Repeat("b", 64),
		}, nil
	}
	ctx := context.Background()
	sess, err := service.CreateRemoteSession(ctx, "network-mismatch", service.request(t, "custody"))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := service.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	runner := &sessionTurnRunner{
		store: service.Store(),
		testNetworkExecutions: func(session.Session) ([]networkstate.Execution, bool, error) {
			return []networkstate.Execution{networkExecutionFixture("run-9", strings.Repeat("f", 64))}, true, nil
		},
	}
	err = runner.sessionNetworkOutcome(bound, "", "session-abc", true)
	if err == nil || !strings.Contains(err.Error(), "run-9") || !strings.Contains(err.Error(), sess.ID) {
		t.Fatalf("mismatch outcome = %v, want a turn failure naming the run", err)
	}
	handler := NewHTTPHandler(service.Service)
	response := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/events", "", "", "")
	var page []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, event := range page {
		if event.Type != string(session.EventNetwork) {
			continue
		}
		var decoded sessionNetworkEventPayload
		if err := json.Unmarshal(event.Payload, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.RunID != "run-9" || len(decoded.Alerts) != 1 || !strings.Contains(decoded.Alerts[0], fingerprint) {
			t.Fatalf("custody event payload = %+v", decoded)
		}
		return
	}
	t.Fatalf("no network event reported the custody failure: %s", response.Body.String())
}

// A run that enforced exactly the session's own policy is not news. It costs no turn and, when
// it observed nothing worth reporting, no event either.
func TestSessionNetworkOutcomeIsSilentForAMatchingQuietRun(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	fingerprint := strings.Repeat("a", 64)
	service.testAdmitNetwork = func(string, string, executionConfig, string, string) (sessionNetworkBinding, error) {
		return sessionNetworkBinding{
			Mode: egress.Filtered, Fingerprint: fingerprint, Qualification: strings.Repeat("b", 64),
		}, nil
	}
	ctx := context.Background()
	sess, err := service.CreateRemoteSession(ctx, "network-quiet", service.request(t, "quiet"))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := service.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	runner := &sessionTurnRunner{
		store: service.Store(),
		testNetworkExecutions: func(session.Session) ([]networkstate.Execution, bool, error) {
			record := networkExecutionFixture("run-1", fingerprint)
			record.AttemptID = "session-abc"
			return []networkstate.Execution{record}, true, nil
		},
	}
	if err := runner.sessionNetworkOutcome(bound, "", "session-abc", true); err != nil {
		t.Fatalf("matching run failed the turn: %v", err)
	}
	handler := NewHTTPHandler(service.Service)
	response := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/events", "", "", "")
	if strings.Contains(response.Body.String(), `"type":"network"`) {
		t.Fatalf("an unsealed, quiet run produced an event: %s", response.Body.String())
	}
}

// A partial inventory cannot prove that no run enforced foreign authority: the record it could
// not read is exactly where such a run would hide. The turn fails closed and says so.
func TestSessionNetworkOutcomeFailsOnIncompleteEvidence(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	fingerprint := strings.Repeat("a", 64)
	service.testAdmitNetwork = func(string, string, executionConfig, string, string) (sessionNetworkBinding, error) {
		return sessionNetworkBinding{Mode: egress.Filtered, Fingerprint: fingerprint, Qualification: strings.Repeat("b", 64)}, nil
	}
	ctx := context.Background()
	sess, err := service.CreateRemoteSession(ctx, "network-incomplete", service.request(t, "partial"))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := service.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	matching := networkExecutionFixture("run-1", fingerprint)
	matching.AttemptID = "session-abc"
	runner := &sessionTurnRunner{
		store: service.Store(),
		testNetworkExecutions: func(session.Session) ([]networkstate.Execution, bool, error) {
			return []networkstate.Execution{matching}, false, nil
		},
	}
	err = runner.sessionNetworkOutcome(bound, "", "session-abc", true)
	if err == nil || !strings.Contains(err.Error(), "network evidence for this session is incomplete") {
		t.Fatalf("incomplete evidence certified the turn: %v", err)
	}
	handler := NewHTTPHandler(service.Service)
	response := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/events", "", "", "")
	if !strings.Contains(response.Body.String(), "is incomplete") {
		t.Fatalf("no network event reported the incomplete inventory: %s", response.Body.String())
	}
	// The same records, read whole, certify the turn.
	runner.testNetworkExecutions = func(session.Session) ([]networkstate.Execution, bool, error) {
		return []networkstate.Execution{matching}, true, nil
	}
	if err := runner.sessionNetworkOutcome(bound, "", "session-abc", true); err != nil {
		t.Fatalf("a complete matching inventory failed the turn: %v", err)
	}
}

// Live connections and one refusal's explanation are the two reads Responder needs but could not
// reach: routing them is what makes the remote view the same view. Both are GET-only projections
// of retained evidence, and both withhold destinations unless the session's policy opted in.
func TestSessionNetworkConnectionAndExplanationRoutes(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "network-parity", service.request(t, "parity"))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(service.Service)
	response := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/network/connections", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("connections status=%d body=%s", response.Code, response.Body.String())
	}
	var connections SessionNetworkConnectionsDTO
	if err := json.Unmarshal(response.Body.Bytes(), &connections); err != nil {
		t.Fatal(err)
	}
	if connections.Status != "not filtered" || connections.Connections == nil || connections.Projection != "destinations-withheld" {
		t.Fatalf("connections = %+v", connections)
	}
	event := strings.Repeat("a", 32)
	response = sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/network/explanations/"+event, "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("explanation status=%d body=%s", response.Code, response.Body.String())
	}
	var explanation SessionNetworkExplanationDTO
	if err := json.Unmarshal(response.Body.Bytes(), &explanation); err != nil {
		t.Fatal(err)
	}
	if explanation.Available || explanation.Reason == "" || explanation.Projection != "destinations-withheld" {
		t.Fatalf("explanation = %+v", explanation)
	}
	// Reads only, and an explanation without an event id is not a route at all.
	for _, path := range []string{"/network/connections", "/network/explanations/" + event} {
		response = sessionHTTPTestRequest(t, handler, http.MethodPost,
			"/v1/sessions/"+sess.ID+path, "{}", "mutate", "application/json")
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status=%d", path, response.Code)
		}
	}
	response = sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+sess.ID+"/network/explanations", "", "", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("explanations index status=%d, want 404", response.Code)
	}
}
