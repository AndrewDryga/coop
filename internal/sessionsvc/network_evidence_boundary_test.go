package sessionsvc

import (
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
)

func newSessionNetworkEvidenceFixture(t *testing.T, export bool) (*sessionFixture, session.Session, *networkstate.Store) {
	t.Helper()
	service, _ := newHTTPTestSessionService(t)
	service.Job.Egress.Mode, service.Job.Egress.ExportDestinations = "filtered", export
	service.testAdmitNetwork = func(jobDigest, sessionID string, _ executionConfig, _, _ string) (sessionNetworkBinding, error) {
		return sessionNetworkBinding{Mode: egress.Filtered, Fingerprint: admitTestNetworkSnapshot(t, jobDigest, sessionID, export), Qualification: strings.Repeat("b", 64)}, nil
	}
	bound, err := service.CreateRemoteSession(context.Background(), "retained-network", service.request(t, "retained-network"))
	if err != nil {
		service.Stop()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Stop() })
	root := filepath.Join(os.Getenv("XDG_STATE_HOME"), "coop", "network")
	store, err := networkstate.OpenExisting(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return service, bound, store
}

func seedSessionNetworkEvidence(t *testing.T, store *networkstate.Store, bound session.Session, eventID, name string) networkstate.Execution {
	t.Helper()
	candidate := networkstate.CandidateSpec{
		Runtime:     networkstate.RuntimeBinding{HostFamily: "linux", Endpoint: "unix:///fixture.sock", DaemonID: "fixture-daemon", OS: "linux", Architecture: "amd64", ServerVersion: "29.4.0", KernelVersion: "fixture"},
		ClientImage: "sha256:" + strings.Repeat("a", 64), GatewayImage: "sha256:" + strings.Repeat("b", 64),
		ClientDefinition: strings.Repeat("c", 64), ClientClosure: strings.Repeat("d", 64), GatewaySource: strings.Repeat("e", 64),
		Libc: "glibc", NodeBase: "node:24@sha256:" + strings.Repeat("f", 64), GoBase: "golang:1.26@sha256:" + strings.Repeat("0", 64),
	}
	smoke, err := store.BeginQualification(candidate, []networkstate.QualifiedClient{{Provider: "claude", Client: egress.ClientACP, Version: "0.75.1"}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := smoke.CreateExecution(context.Background(), networkstate.ExecutionSpec{
		Project: bound.Repository, JobDigest: bound.JobDigest, SessionID: bound.ID, AttemptID: "attempt", PolicyFingerprint: bound.NetworkFingerprint,
		Runtime: "docker", DaemonID: candidate.Runtime.DaemonID, Endpoint: candidate.Runtime.Endpoint, ClientImage: candidate.ClientImage, GatewayImage: candidate.GatewayImage,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := record.Snapshot
	snapshot.Sequence, snapshot.AsOf, snapshot.Availability = 1, time.Now().UTC(), "available"
	snapshot.Scope = "proxy-streams-and-sampled-tcp-sockets"
	snapshot.Connections = []networkview.Connection{{ID: strings.Repeat("a", 32), DestinationID: strings.Repeat("b", 32), State: "open", Transport: "tcp", Name: name, NameSource: "sni", Service: "web", Peer: "203.0.113.1:443", ObservedAt: snapshot.AsOf}}
	port := 443
	snapshot.Denials = []networkview.Denial{{ID: eventID, Source: "guard", Sequence: 1, Basis: "observed", Kind: "tls_denied", Reason: "unapproved_name", Name: name, Service: "web", Port: &port, At: snapshot.AsOf,
		Candidate: &networkview.Candidate{ID: strings.Repeat("c", 32), EvidenceID: eventID, PolicyFingerprint: snapshot.PolicyFingerprint, Rule: egress.Rule{To: egress.Destination{Domain: name}, Protocol: "tls", Ports: []int{443}}, AppliesTo: "next_run"}}}
	record, err = store.AcceptSnapshot(context.Background(), record.ID, record.Revision, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestSessionNetworkRetainedReadsRespectSessionAndProjection(t *testing.T) {
	for _, export := range []bool{false, true} {
		name := "withheld"
		if export {
			name = "exported"
		}
		t.Run(name, func(t *testing.T) {
			service, bound, store := newSessionNetworkEvidenceFixture(t, export)
			handler := NewHTTPHandler(service.Service)
			var bodies []string
			get := func(suffix string, value any) {
				t.Helper()
				response := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+bound.ID+suffix, "", "", "")
				if response.Code != http.StatusOK {
					t.Fatalf("GET %s = %d %s", suffix, response.Code, response.Body.String())
				}
				if err := json.Unmarshal(response.Body.Bytes(), value); err != nil {
					t.Fatal(err)
				}
				bodies = append(bodies, response.Body.String())
			}
			var before SessionNetworkDTO
			get("/network", &before)
			if before.Current.Status != "no run yet" {
				t.Fatalf("empty inventory = %+v", before.Current)
			}
			ownID, foreignID := strings.Repeat("d", 32), strings.Repeat("e", 32)
			own := seedSessionNetworkEvidence(t, store, bound, ownID, "owned.example.org")
			foreign := bound
			foreign.ID = "foreign-session"
			policy, err := store.CaptureJob(networkstate.JobSnapshotRef{JobDigest: foreign.JobDigest, SessionID: foreign.ID}, nil, nil, export)
			if err != nil {
				t.Fatal(err)
			}
			foreign.NetworkFingerprint = policy.Fingerprint
			other := seedSessionNetworkEvidence(t, store, foreign, foreignID, "foreign.example.org")
			if !other.StartedAt.After(own.StartedAt) {
				t.Fatal("foreign fixture must be newest")
			}
			var view SessionNetworkDTO
			get("/network", &view)
			if view.Current.RunID != own.ID || view.Fingerprint != bound.NetworkFingerprint {
				t.Fatalf("wrong run: %+v", view)
			}
			var connections SessionNetworkConnectionsDTO
			get("/network/connections", &connections)
			if connections.RunID != own.ID || len(connections.Connections) != 1 || connections.Connections[0].Service != "web" {
				t.Fatalf("lost observation: %+v", connections)
			}
			var explanation SessionNetworkExplanationDTO
			get("/network/explanations/"+ownID, &explanation)
			if !explanation.Available || explanation.Explanation == nil || explanation.Explanation.RunID != own.ID || explanation.Explanation.PolicyFingerprint != bound.NetworkFingerprint || explanation.Explanation.Event.ID != ownID || explanation.Explanation.Event.Reason != "unapproved_name" {
				t.Fatalf("lost refusal: %+v", explanation)
			}
			if export {
				if connections.Projection != "destinations-included" || connections.Connections[0].Name != "owned.example.org" || explanation.Explanation.Event.Candidate == nil || explanation.Explanation.CandidateState != "draft_not_approved" {
					t.Fatalf("export withheld: %+v %+v", connections, explanation)
				}
			} else if connections.Projection != "destinations-withheld" || connections.Connections[0].Name != "" || connections.Connections[0].Peer != "" || explanation.Explanation.Event.Candidate != nil || explanation.Explanation.CandidateState != "withheld" {
				t.Fatalf("private evidence escaped: %+v %+v", connections, explanation)
			}
			var missing SessionNetworkExplanationDTO
			get("/network/explanations/"+foreignID, &missing)
			if missing.Available || missing.Explanation != nil || missing.Reason != "event_not_retained" {
				t.Fatalf("foreign event: %+v", missing)
			}
			wire := strings.Join(bodies, "\n")
			for _, private := range []string{"foreign.example.org", other.ID, bound.Repository, bound.Workspace, "unix:///fixture.sock", "fixture-daemon"} {
				if strings.Contains(wire, private) {
					t.Fatalf("exposed private value %q", private)
				}
			}
			if !export && strings.Contains(wire, "owned.example.org") {
				t.Fatal("withheld destination exposed")
			}
		})
	}
}

func TestSessionNetworkInventoryReadErrorFailsCertification(t *testing.T) {
	service, bound, _ := newSessionNetworkEvidenceFixture(t, false)
	const cause = "private-inventory-error-canary"
	runner := &sessionTurnRunner{store: service.Store(), testNetworkExecutions: func(session.Session) ([]networkstate.Execution, bool, error) { return nil, false, errors.New(cause) }}
	err := runner.sessionNetworkOutcome(bound, "", "attempt", true)
	var failure *sessionACPFailure
	if !errors.As(err, &failure) || failure.code != sessionACPProcessError {
		t.Fatalf("unreadable inventory certified turn: %v", err)
	}
	response := sessionHTTPTestRequest(t, NewHTTPHandler(service.Service), http.MethodGet, "/v1/sessions/"+bound.ID+"/events", "", "", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"network"`) {
		t.Fatalf("missing failure event: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), cause) {
		t.Fatal("private read error exposed")
	}
}

func TestSessionNetworkUnreadableInventoryDoesNotInventAbsence(t *testing.T) {
	for _, readableRun := range []bool{false, true} {
		name := "all-unreadable"
		if readableRun {
			name = "older-readable-run"
		}
		t.Run(name, func(t *testing.T) {
			service, bound, store := newSessionNetworkEvidenceFixture(t, false)
			if readableRun {
				seedSessionNetworkEvidence(t, store, bound, strings.Repeat("d", 32), "owned.example.org")
			}
			broken := filepath.Join(store.Path(), "execution-"+strings.Repeat("f", 32)+".json")
			if err := os.WriteFile(broken, []byte("{broken"), 0600); err != nil {
				t.Fatal(err)
			}
			handler := NewHTTPHandler(service.Service)
			for _, suffix := range []string{"/network", "/network/connections", "/network/explanations/" + strings.Repeat("e", 32)} {
				response := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+bound.ID+suffix, "", "", "")
				if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"network_unavailable"`) {
					t.Fatalf("incomplete GET %s = %d %s", suffix, response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestSessionNetworkExplanationReadErrorIsNotRetention(t *testing.T) {
	service, bound, store := newSessionNetworkEvidenceFixture(t, false)
	eventID := strings.Repeat("d", 32)
	record := seedSessionNetworkEvidence(t, store, bound, eventID, "owned.example.org")
	snapshot := record.Snapshot
	snapshot.Sequence++
	snapshot.Denials[0].Sequence = 0
	if _, err := store.AcceptSnapshot(context.Background(), record.ID, record.Revision, snapshot); err != nil {
		t.Fatal(err)
	}
	response := sessionHTTPTestRequest(t, NewHTTPHandler(service.Service), http.MethodGet, "/v1/sessions/"+bound.ID+"/network/explanations/"+eventID, "", "", "")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"network_unavailable"`) {
		t.Fatalf("invalid decision became retention: %d %s", response.Code, response.Body.String())
	}
}
