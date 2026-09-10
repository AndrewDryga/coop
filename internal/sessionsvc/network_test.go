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
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

const testSessionPolicyHeader = "version: 1\npolicies:\n  responder:\n    repository: %s\n" +
	"    target: codex@work\n    max_turns: 10\n    max_queued_turns: 5\n" +
	"    max_queued_bytes: 4096\n    max_patch_bytes: 8192\n    turn_timeout: 1h\n"

func parseNetworkPolicy(t *testing.T, repo, block string) (Policy, error) {
	t.Helper()
	body := strings.Replace(testSessionPolicyHeader, "%s", repo, 1) + block
	policies, err := parseSessionPolicies([]byte(body), nil)
	if err != nil {
		return Policy{}, err
	}
	return policies["responder"], nil
}

// The `egress:` block is operator authority written by hand, so it is parsed strictly: an unknown
// key is a typo that would otherwise silently grant nothing, and rules under a posture that cannot
// enforce them are a contradiction, not a preference.
func TestSessionPolicyEgressParsesStrictly(t *testing.T) {
	repo := realGitRepoFixture(t)
	for name, test := range map[string]struct {
		block  string
		reject string
	}{
		"unknown field": {
			block:  "    egress:\n      mode: filtered\n      allow: [example.com]\n",
			reject: "field allow not found",
		},
		"missing mode": {
			block:  "    egress:\n      export_destinations: true\n",
			reject: "egress.mode is required",
		},
		"invalid mode": {
			block:  "    egress:\n      mode: partial\n",
			reject: "egress.mode",
		},
		"rules without filtered": {
			block:  "    egress:\n      mode: open\n      rules:\n        - to: {domain: example.com}\n          protocol: tls\n          ports: [443]\n",
			reject: "egress.rules require egress.mode: filtered",
		},
		"unknown rule field": {
			block:  "    egress:\n      mode: filtered\n      rules:\n        - to: {domain: example.com}\n          transport: tls\n",
			reject: "unknown, duplicate or null field",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseNetworkPolicy(t, repo, test.block); err == nil ||
				!strings.Contains(err.Error(), test.reject) {
				t.Fatalf("error = %v, want one naming %q", err, test.reject)
			}
		})
	}

	policy, err := parseNetworkPolicy(t, repo,
		"    egress:\n      mode: filtered\n      export_destinations: true\n"+
			"      rules:\n        - to: {domain: example.com}\n          protocol: tls\n          ports: [443]\n")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Egress.Mode != egress.Filtered || !policy.Egress.ExportDestinations ||
		len(policy.Egress.Rules) != 1 || policy.Egress.Rules[0].To.Domain != "example.com" {
		t.Fatalf("parsed egress = %+v", policy.Egress)
	}
}

// Network reach is authority, and it is also part of the policy's identity: an edit has to move
// BOTH digests, or a controller that pinned either one would keep launching under a policy whose
// destinations changed under it. A policy with no block must keep its pre-networking digests.
func TestSessionPolicyEgressBindsIntoBothDigests(t *testing.T) {
	repo := realGitRepoFixture(t)
	silent, err := parseNetworkPolicy(t, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	// A policy file written before `egress:` existed must keep the digests it already had, or
	// every session created under it would stop matching its own policy. That holds because an
	// unwritten block contributes NOTHING to the canonical form, not because it digests as open.
	if digestedSessionEgress(silent.Egress) != nil {
		t.Fatal("a policy with no egress block contributed a network block to its digests")
	}
	for name, block := range map[string]string{
		// `mode: open` is explicit operator authority and a silent policy is not, so even the
		// posture that resolves the same way has to be a distinguishable policy.
		"explicit open": "    egress:\n      mode: open\n",
		"filtered":      "    egress:\n      mode: filtered\n",
		"offline":       "    egress:\n      mode: none\n",
		"one rule":      "    egress:\n      mode: filtered\n      rules:\n        - to: {domain: example.com}\n          protocol: tls\n          ports: [443]\n",
		"another rule":  "    egress:\n      mode: filtered\n      rules:\n        - to: {domain: other.example}\n          protocol: tls\n          ports: [443]\n",
		"exported":      "    egress:\n      mode: filtered\n      export_destinations: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			edited, err := parseNetworkPolicy(t, repo, block)
			if err != nil {
				t.Fatal(err)
			}
			if ResolvedPolicyDigest(edited) == ResolvedPolicyDigest(silent) {
				t.Fatalf("%s left the policy digest unchanged", name)
			}
			if ResolvedPolicyAuthorityDigest(edited) == ResolvedPolicyAuthorityDigest(silent) {
				t.Fatalf("%s left the authority digest unchanged", name)
			}
		})
	}
}

// Creation freezes the posture on the immutable row. Every later run reads it from there, so if
// it were not persisted the session would silently fall back to open on the next turn.
func TestCreateFreezesTheSessionNetworkBinding(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	service.testAdmitNetwork = func(_ Policy, workspace, forkName string) (sessionNetworkBinding, error) {
		if workspace == "" || forkName == "" {
			t.Errorf("admission ran without a workspace (%q) or fork (%q)", workspace, forkName)
		}
		return sessionNetworkBinding{
			Mode: egress.Filtered, Fingerprint: strings.Repeat("a", 64), Qualification: strings.Repeat("b", 64),
		}, nil
	}
	ctx := context.Background()
	sess, err := service.CreateRemoteSession(ctx, "create-network", CreateRemoteSessionRequest{
		Policy: "responder", Task: "freeze the posture",
	})
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
	handler := NewHTTPHandler(service)
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
	service.testAdmitNetwork = func(Policy, string, string) (sessionNetworkBinding, error) {
		return sessionNetworkBinding{}, errNetworkFixture
	}
	_, err := service.CreateRemoteSession(context.Background(), "create-network-refused", CreateRemoteSessionRequest{
		Policy: "responder", Task: "refuse the posture",
	})
	if err == nil || session.CodeOf(err) != session.CodeNetworkUnavailable ||
		!strings.Contains(err.Error(), "this host is not set up for filtered runs") {
		t.Fatalf("create error = %v (code %q)", err, session.CodeOf(err))
	}
}

var errNetworkFixture = &session.Error{
	Code: session.CodeInvalidRequest, Detail: "this host is not set up for filtered runs with this Docker and these agents",
}

// The child receives the capture from its host parent and nothing else. An open session must be
// launched exactly as it is at HEAD: no COOP_EGRESS override, no capture.
func TestNetworkChildEnvironmentCarriesOnlyAFrozenPosture(t *testing.T) {
	open := session.Session{ID: "remote_1", Repository: "/srv/app", NetworkMode: "open"}
	if env, err := networkChildEnvironment(open, "session-abc"); err != nil || len(env) != 0 {
		t.Fatalf("open session env = %v, err %v", env, err)
	}
	offline := open
	offline.NetworkMode = "none"
	env, err := networkChildEnvironment(offline, "session-abc")
	if err != nil || len(env) != 1 || env[0] != "COOP_EGRESS=none" {
		t.Fatalf("offline session env = %v, err %v", env, err)
	}
	filtered := session.Session{
		ID: "remote_1", Repository: "/srv/app", NetworkMode: "filtered",
		NetworkFingerprint: strings.Repeat("a", 64), NetworkQualification: strings.Repeat("b", 64),
	}
	env, err = networkChildEnvironment(filtered, "session-abc")
	if err != nil || len(env) != 2 || env[0] != "COOP_EGRESS=filtered" {
		t.Fatalf("filtered session env = %v, err %v", env, err)
	}
	value, ok := strings.CutPrefix(env[1], "COOP_NETWORK_CAPTURE=")
	if !ok {
		t.Fatalf("filtered session env = %v", env)
	}
	var capture struct {
		Project       string `json:"project"`
		Fingerprint   string `json:"fingerprint"`
		Qualification string `json:"qualification"`
		SessionID     string `json:"session_id"`
		AttemptID     string `json:"attempt_id"`
	}
	if err := json.Unmarshal([]byte(value), &capture); err != nil {
		t.Fatal(err)
	}
	if capture.Project != "/srv/app" || capture.Fingerprint != filtered.NetworkFingerprint ||
		capture.Qualification != filtered.NetworkQualification ||
		capture.SessionID != "remote_1" || capture.AttemptID != "session-abc" {
		t.Fatalf("capture = %+v", capture)
	}
	// A filtered session with no captured policy is a broken row, not an open launch.
	broken := filtered
	broken.NetworkFingerprint = ""
	if _, err := networkChildEnvironment(broken, "session-abc"); err == nil {
		t.Fatal("a filtered session with no fingerprint produced a launch environment")
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
	sess, err := service.CreateRemoteSession(context.Background(), "open-network", CreateRemoteSessionRequest{
		Policy: "responder", Task: "open session",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(service)
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
			service, repo := newHTTPTestSessionService(t)
			defer service.Stop()
			fingerprint := admitTestNetworkSnapshot(t, repo, export)
			service.testAdmitNetwork = func(Policy, string, string) (sessionNetworkBinding, error) {
				return sessionNetworkBinding{
					Mode: egress.Filtered, Fingerprint: fingerprint, Qualification: strings.Repeat("b", 64),
				}, nil
			}
			sess, err := service.CreateRemoteSession(context.Background(), "filtered-network", CreateRemoteSessionRequest{
				Policy: "responder", Task: "filtered session",
			})
			if err != nil {
				t.Fatal(err)
			}
			handler := NewHTTPHandler(service)
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
	sess, err := service.CreateRemoteSession(ctx, "network-event", CreateRemoteSessionRequest{
		Policy: "responder", Task: "network event",
	})
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
	handler := NewHTTPHandler(service)
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
func admitTestNetworkSnapshot(t *testing.T, repo string, export bool) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := filepath.Join(os.Getenv("XDG_STATE_HOME"), "coop", "network")
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	mode := egress.Filtered
	rules, err := egress.NormalizeRules([]egress.Rule{
		{To: egress.Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{443}},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := store.Admit(canonical, networkstate.Admission{
		InvocationMode: &mode, ExportDestinations: export,
		Operator: []egress.Input{{Origin: egress.Origin{Kind: "operator", Name: "session-policy"}, Rules: rules}},
	})
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

// realGitRepoFixture is a policy-shaped repository path: absolute, clean, and its own real
// directory, which is what LoadPolicies requires and what a macOS temp dir is not.
func realGitRepoFixture(t *testing.T) string {
	t.Helper()
	repo, _ := gitrepo.New(t)
	real, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	return real
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
	service.testAdmitNetwork = func(Policy, string, string) (sessionNetworkBinding, error) {
		return sessionNetworkBinding{
			Mode: egress.Filtered, Fingerprint: fingerprint, Qualification: strings.Repeat("b", 64),
		}, nil
	}
	ctx := context.Background()
	sess, err := service.CreateRemoteSession(ctx, "network-mismatch", CreateRemoteSessionRequest{
		Policy: "responder", Task: "custody",
	})
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
	handler := NewHTTPHandler(service)
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
	service.testAdmitNetwork = func(Policy, string, string) (sessionNetworkBinding, error) {
		return sessionNetworkBinding{
			Mode: egress.Filtered, Fingerprint: fingerprint, Qualification: strings.Repeat("b", 64),
		}, nil
	}
	ctx := context.Background()
	sess, err := service.CreateRemoteSession(ctx, "network-quiet", CreateRemoteSessionRequest{
		Policy: "responder", Task: "quiet",
	})
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
	handler := NewHTTPHandler(service)
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
	service.testAdmitNetwork = func(Policy, string, string) (sessionNetworkBinding, error) {
		return sessionNetworkBinding{Mode: egress.Filtered, Fingerprint: fingerprint, Qualification: strings.Repeat("b", 64)}, nil
	}
	ctx := context.Background()
	sess, err := service.CreateRemoteSession(ctx, "network-incomplete", CreateRemoteSessionRequest{Policy: "responder", Task: "partial"})
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
	handler := NewHTTPHandler(service)
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
	sess, err := service.CreateRemoteSession(context.Background(), "network-parity", CreateRemoteSessionRequest{
		Policy: "responder", Task: "parity",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(service)
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
