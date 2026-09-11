package workerproto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func text(value string) *string { return &value }

func port(value int) *int { return &value }

func at(value string) *time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return &parsed
}

func sampleCoverage(status string) *NetworkCoverage {
	metric := NetworkMetricCoverage{Status: status}
	return &NetworkCoverage{ProxyBytes: metric, Connections: metric, UpstreamFailures: metric,
		KernelPackets: metric, GuardDenials: metric, MaintenanceQueries: metric,
		MaintenanceBytes: metric, SocketInventory: metric, BoundaryAttribution: metric}
}

// sampleSessionEvidence is an AUTHORED specimen, not a harvested production capture: no filtered
// fleet session had exported evidence before this contract existed. Its values mirror the shapes
// the daemon's own network and task tests produce (a withheld projection, one denial, counters
// above 2^53, a two-run receipt, a bound task with a partly checked list).
func sampleSessionEvidence() SessionEvidence {
	return SessionEvidence{
		Version: SessionEvidenceVersion, CapturedAt: time.Date(2026, 9, 11, 14, 32, 40, 0, time.UTC),
		SessionID: "remote_01j9zq3f8m0c7e6kq9y2s4x1nt", Revision: 7, State: "open",
		Network: NetworkEvidence{
			Mode: NetworkModeFiltered, Fingerprint: text(strings.Repeat("a", 64)),
			Access: NetworkAccess{
				Status: EvidenceStatusCaptured, Qualification: text(strings.Repeat("b", 64)),
				Projection: text(NetworkProjectionWithheld), Requested: []string{}, Effective: []string{},
			},
			Observation: NetworkObservation{
				Status: EvidenceStatusObserved, Freshness: text("terminal"), RunID: text("run-7f3a"),
				AttemptID: text("attempt-2"), GatewayEpoch: text("epoch-1"), Sequence: text("42"),
				AsOf: at("2026-09-11T14:32:39Z"), Availability: text("available"),
				Scope: text("proxy-streams-and-sampled-tcp-sockets"), Sealed: true,
				CleanupOutcome: text("complete"), Projection: text(NetworkProjectionWithheld),
				Health: &NetworkHealth{
					Enforcer: NetworkLayerHealth{Status: "ok"}, Gateway: NetworkLayerHealth{Status: "ok"},
					Resolver:  NetworkLayerHealth{Status: "ok"},
					Collector: NetworkLayerHealth{Status: "degraded", Reason: text("socket_sample_lag")},
				},
				Coverage: sampleCoverage("exact"),
				Counters: &NetworkCounters{
					SentBytes: text("184320"), ReceivedBytes: text("13002342"), Connections: text("9"),
					UpstreamFailures: text("0"), DeniedPackets: text("18446744073709551615"), ProtectedPackets: text("0"),
					DeniedDNSQueries: text("0"), DeniedTLSConnections: text("2"), MaintenanceQueries: text("3"),
					MaintenanceFailures: text("0"), IngressDeniedPackets: text("0"),
					MaintenanceSentBytes: nil, MaintenanceReceivedBytes: nil,
				},
				Loss: &NetworkLoss{Records: "0", Reasons: []string{}, SuppressedAlerts: "0"},
				Sources: []NetworkSource{{
					ID: "gateway", Status: "sealed", Sequence: "42", ObservedAt: at("2026-09-11T14:32:39Z"),
					LastEventAt: at("2026-09-11T14:32:20Z"), LostRecords: "0",
				}},
				Denials: []NetworkDenial{{
					ID: "evt-0031", At: time.Date(2026, 9, 11, 14, 32, 20, 0, time.UTC), Kind: "tls_denied",
					Basis: "tls", Reason: "no_matching_rule", Source: "gateway", SourceSequence: "31",
					DestinationWithheld: true, Port: port(443),
				}},
				Connections: []NetworkConnection{{
					ID: "conn-0009", State: "closed", Transport: "tcp", DestinationWithheld: true,
					StartedAt: at("2026-09-11T14:31:58Z"), ObservedAt: time.Date(2026, 9, 11, 14, 32, 39, 0, time.UTC),
					SentBytes: text("2048"), ReceivedBytes: text("917504"),
				}},
				Alerts: []NetworkAlert{{
					ID: "alert-0002", Category: "collector_health", Severity: "warning", State: "raised",
					FirstSeen: time.Date(2026, 9, 11, 14, 32, 10, 0, time.UTC), LastSeen: time.Date(2026, 9, 11, 14, 32, 39, 0, time.UTC),
					Reason: text("socket_sample_lag"), HealthStatus: text("degraded"),
				}},
				OmittedAlerts: 1,
			},
			Receipt: NetworkReceipt{
				Status: EvidenceStatusAvailable, PolicyFingerprint: text(strings.Repeat("a", 64)),
				AuthorityDigest: text(strings.Repeat("c", 64)), StartedAt: at("2026-09-11T14:20:00Z"),
				Finality: text("provisional"), Completeness: text("partial"), Scope: text("proxy-streams-and-sampled-tcp-sockets"),
				Counters: &NetworkCounters{
					SentBytes: text("190464"), ReceivedBytes: text("13002342"), Connections: text("11"),
					UpstreamFailures: text("0"), DeniedPackets: text("18446744073709551615"), ProtectedPackets: text("0"),
					DeniedDNSQueries: text("0"), DeniedTLSConnections: text("2"), MaintenanceQueries: text("5"),
					MaintenanceFailures: text("0"), IngressDeniedPackets: text("0"),
				},
				Coverage: sampleCoverage("lower-bound"),
				Loss:     &NetworkLoss{Records: "0", Unknown: true, Reasons: []string{"counter_overflow"}, SuppressedAlerts: "0"},
				Runs: []NetworkRunReference{
					{RunID: "run-2b91", GatewayEpoch: "epoch-1", Sequence: "12", AsOf: time.Date(2026, 9, 11, 14, 25, 0, 0, time.UTC),
						Finality: "final", Completeness: "complete", ReceiptDigest: strings.Repeat("d", 64)},
					{RunID: "run-7f3a", GatewayEpoch: "epoch-1", Sequence: "42", AsOf: time.Date(2026, 9, 11, 14, 32, 39, 0, time.UTC),
						Finality: "final", Completeness: "complete", ReceiptDigest: strings.Repeat("e", 64)},
				},
				RunCount: text("2"), OmittedRunReferences: text("0"), Projection: text(NetworkProjectionWithheld),
				ReceiptDigest: text(strings.Repeat("f", 64)), DigestScope: text(NetworkProjectionWithheld),
			},
		},
		Task: TaskEvidence{
			Status: EvidenceStatusBound, QueueID: text(strings.Repeat("1", 32)), TaskID: text(strings.Repeat("2", 32)),
			ID: text("responder-0a1b2c3d4e5f60718293a4b5"), OfferRef: text("offer:episode-42:task"),
			DraftSHA256: text(strings.Repeat("3", 64)),
			Snapshot: &TaskSnapshot{
				State: "in_progress", StateSHA256: strings.Repeat("4", 64), Title: "Fix API timeout",
				Checklist: []TaskChecklistItem{
					{Label: "Reproduce the timeout with the recorded request", Checked: true},
					{Label: "Raise the upstream deadline and add the regression", Checked: true},
					{Label: "Run the owning package tests", Checked: true},
					{Label: "Run make dev-check", Checked: false},
				},
				Files: []TaskFile{
					{Path: ".coop-queue.json", ByteSize: 96, SHA256: strings.Repeat("5", 64)},
					{Path: "10_in_progress/responder-0a1b2c3d4e5f60718293a4b5/log.md", ByteSize: 812, SHA256: strings.Repeat("6", 64)},
					{Path: "10_in_progress/responder-0a1b2c3d4e5f60718293a4b5/state.md", ByteSize: 240, SHA256: strings.Repeat("7", 64)},
					{Path: "10_in_progress/responder-0a1b2c3d4e5f60718293a4b5/task.md", ByteSize: 1930, SHA256: strings.Repeat("8", 64)},
				},
				StateNote: TaskNote{Status: EvidenceStatusCaptured, Text: text("# State — Fix API timeout\n\nStatus: in progress — deadline raised, gate not run yet.\n")},
			},
		},
	}
}

// sampleOpenSessionEvidence is the other honest shape: an open session has no capture, no
// observation and no receipt, and a session without a workspace task has nothing bound.
func sampleOpenSessionEvidence() SessionEvidence {
	return SessionEvidence{
		Version: SessionEvidenceVersion, CapturedAt: time.Date(2026, 9, 11, 9, 30, 0, 0, time.UTC),
		SessionID: "remote_01j9zpx0v2b8d3g5h7k9m1n3p5", Revision: 2, State: "closed",
		Network: NetworkEvidence{
			Mode:   NetworkModeOpen,
			Access: NetworkAccess{Status: EvidenceStatusNotFiltered, Requested: []string{}, Effective: []string{}},
			Observation: NetworkObservation{Status: EvidenceStatusNotFiltered, Sources: []NetworkSource{},
				Denials: []NetworkDenial{}, Connections: []NetworkConnection{}, Alerts: []NetworkAlert{}},
			Receipt: NetworkReceipt{Status: EvidenceStatusNotFiltered, Runs: []NetworkRunReference{}},
		},
		Task: TaskEvidence{Status: EvidenceStatusUnbound},
	}
}

// Responder decodes these objects; their field names, null shapes and RFC3339 UTC stamps are the
// contract. The golden fixtures are the only thing that fails when a rename breaks a control plane
// that is not in this repository.
func TestSessionEvidenceMarshalsTheExactPublishedShape(t *testing.T) {
	for name, sample := range map[string]SessionEvidence{
		"session_evidence.json":      sampleSessionEvidence(),
		"session_evidence_open.json": sampleOpenSessionEvidence(),
	} {
		encoded, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(encoded), storageFixture(t, name); got != want {
			t.Fatalf("%s wire shape drifted:\n got %s\nwant %s", name, got, want)
		}
		if err := sample.Validate(); err != nil {
			t.Fatalf("the published %s fixture failed validation: %v", name, err)
		}
	}
}

// Unknown is not zero: a null counter, a null unattributed loss and an unbound task all have to
// survive the round trip as absences, and the decode has to be as strict as the poll's.
func TestSessionEvidenceRoundTripsAbsencesAndRefusesUnknownFields(t *testing.T) {
	decoded, err := DecodeSessionEvidence([]byte(storageFixture(t, "session_evidence.json")))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Network.Observation.Counters.MaintenanceSentBytes != nil {
		t.Fatalf("an unmeasured counter decoded as %q, want unknown", *decoded.Network.Observation.Counters.MaintenanceSentBytes)
	}
	if got := *decoded.Network.Observation.Counters.DeniedPackets; got != "18446744073709551615" {
		t.Fatalf("a 64-bit counter lost exactness: %s", got)
	}
	if decoded.Network.Observation.Denials[0].Destination != nil || !decoded.Network.Observation.Denials[0].DestinationWithheld {
		t.Fatalf("a withheld destination leaked: %+v", decoded.Network.Observation.Denials[0])
	}
	open, err := DecodeSessionEvidence([]byte(storageFixture(t, "session_evidence_open.json")))
	if err != nil {
		t.Fatal(err)
	}
	if open.Task.Snapshot != nil || open.Task.QueueID != nil || open.Network.Fingerprint != nil {
		t.Fatalf("an open session invented evidence: %+v", open)
	}
	extra := strings.Replace(storageFixture(t, "session_evidence_open.json"), `"version":1,`, `"version":1,"host_path":"/home/coop",`, 1)
	if _, err := DecodeSessionEvidence([]byte(extra)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestSessionEvidenceValidateRejectsIncoherentEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*SessionEvidence){
		"unsupported version":              func(e *SessionEvidence) { e.Version = 2 },
		"missing capture time":             func(e *SessionEvidence) { e.CapturedAt = time.Time{} },
		"zero revision":                    func(e *SessionEvidence) { e.Revision = 0 },
		"unknown session state":            func(e *SessionEvidence) { e.State = "parked" },
		"unknown network mode":             func(e *SessionEvidence) { e.Network.Mode = "maybe" },
		"filtered without fingerprint":     func(e *SessionEvidence) { e.Network.Fingerprint = nil },
		"open with fingerprint":            func(e *SessionEvidence) { e.Network.Mode = NetworkModeOpen },
		"access posture mismatch":          func(e *SessionEvidence) { e.Network.Access.Status = EvidenceStatusNotFiltered },
		"unavailable access without cause": func(e *SessionEvidence) { e.Network.Access.Status = EvidenceStatusUnavailable },
		"withheld access naming rules": func(e *SessionEvidence) {
			e.Network.Access.Requested = []string{"example.com tls/443"}
		},
		"included rules under withheld observation": func(e *SessionEvidence) {
			e.Network.Observation.Denials[0].Destination = text("api.example.com")
		},
		"withheld flag under included projection": func(e *SessionEvidence) {
			e.Network.Observation.Projection = text(NetworkProjectionIncluded)
		},
		"counter with sign":           func(e *SessionEvidence) { e.Network.Observation.Counters.SentBytes = text("-1") },
		"counter with leading zero":   func(e *SessionEvidence) { e.Network.Observation.Counters.SentBytes = text("01") },
		"counter above 64 bits":       func(e *SessionEvidence) { e.Network.Observation.Counters.SentBytes = text("18446744073709551616") },
		"unknown coverage status":     func(e *SessionEvidence) { e.Network.Observation.Coverage.ProxyBytes.Status = "guessed" },
		"unknown denial basis":        func(e *SessionEvidence) { e.Network.Observation.Denials[0].Basis = "vibes" },
		"denial port out of range":    func(e *SessionEvidence) { e.Network.Observation.Denials[0].Port = port(70000) },
		"observation without run id":  func(e *SessionEvidence) { e.Network.Observation.RunID = nil },
		"observation without as-of":   func(e *SessionEvidence) { e.Network.Observation.AsOf = nil },
		"negative omitted count":      func(e *SessionEvidence) { e.Network.Observation.OmittedDenials = -1 },
		"no run carrying a counter":   func(e *SessionEvidence) { e.Network.Observation.Status = EvidenceStatusNoRun },
		"alert seen before first":     func(e *SessionEvidence) { e.Network.Observation.Alerts[0].LastSeen = time.Time{} },
		"receipt final without close": func(e *SessionEvidence) { e.Network.Receipt.Finality = text("final") },
		"receipt digest scope drift":  func(e *SessionEvidence) { e.Network.Receipt.DigestScope = text(NetworkProjectionIncluded) },
		"receipt without coverage":    func(e *SessionEvidence) { e.Network.Receipt.Coverage = nil },
		"receipt run without digest":  func(e *SessionEvidence) { e.Network.Receipt.Runs[0].ReceiptDigest = "short" },
		"unavailable receipt with aggregate": func(e *SessionEvidence) {
			e.Network.Receipt.Status = EvidenceStatusUnavailable
			e.Network.Receipt.Reason = text("registry unreadable")
		},
		"bound task without snapshot": func(e *SessionEvidence) { e.Task.Snapshot = nil },
		"bound task without offer":    func(e *SessionEvidence) { e.Task.OfferRef = nil },
		"unbound task with identity":  func(e *SessionEvidence) { e.Task.Status = EvidenceStatusUnbound },
		"unavailable task without cause": func(e *SessionEvidence) {
			e.Task.Status = EvidenceStatusUnavailable
			e.Task.Snapshot = nil
		},
		"unknown task state":     func(e *SessionEvidence) { e.Task.Snapshot.State = "someday" },
		"task without files":     func(e *SessionEvidence) { e.Task.Snapshot.Files = nil },
		"task file with NUL":     func(e *SessionEvidence) { e.Task.Snapshot.Files[0].Path = "a\x00b" },
		"blank checklist label":  func(e *SessionEvidence) { e.Task.Snapshot.Checklist[0].Label = "   " },
		"captured note no text":  func(e *SessionEvidence) { e.Task.Snapshot.StateNote.Text = nil },
		"withheld note no cause": func(e *SessionEvidence) { e.Task.Snapshot.StateNote = TaskNote{Status: EvidenceStatusWithheld} },
		"oversized note": func(e *SessionEvidence) {
			e.Task.Snapshot.StateNote.Text = text(strings.Repeat("x", MaxSessionEvidenceNoteBytes+1))
		},
	} {
		evidence := sampleSessionEvidence()
		mutate(&evidence)
		if err := evidence.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, mutate := range map[string]func(*SessionEvidence){
		"open session with a task snapshot": func(e *SessionEvidence) { e.Task = sampleSessionEvidence().Task },
		"unavailable task with a reason": func(e *SessionEvidence) {
			e.Task = sampleSessionEvidence().Task
			e.Task.Status = EvidenceStatusUnavailable
			e.Task.Snapshot = nil
			e.Task.Reason = text("bound workspace task is missing")
		},
		"filtered session whose registry could not be read": func(e *SessionEvidence) {
			filtered := sampleSessionEvidence()
			e.Network = filtered.Network
			e.Network.Access = NetworkAccess{Status: EvidenceStatusUnavailable, Reason: text("network registry unreadable"),
				Requested: []string{}, Effective: []string{}}
			e.Network.Observation = NetworkObservation{Status: EvidenceStatusUnavailable, Reason: text("network registry unreadable"),
				Sources: []NetworkSource{}, Denials: []NetworkDenial{}, Connections: []NetworkConnection{}, Alerts: []NetworkAlert{}}
			e.Network.Receipt = NetworkReceipt{Status: EvidenceStatusUnavailable, Reason: text("network registry unreadable"),
				Runs: []NetworkRunReference{}}
		},
	} {
		evidence := sampleOpenSessionEvidence()
		mutate(&evidence)
		if err := evidence.Validate(); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}

// The command is on the protocol allowlist, and its result is exactly the evidence object.
func TestWorkerProtocolAdmitsTheSessionEvidenceCommand(t *testing.T) {
	command := Command{
		CommandID: "command-1", WorkerID: "worker-a", SessionRef: "session-a", PlacementGeneration: 1,
		LeaseRef: "lease-1", LeaseExpiresAt: time.Date(2026, 9, 11, 4, 5, 6, 0, time.UTC),
		Kind: "get_session_evidence", CommandVersion: Version, Payload: json.RawMessage(`{"coop_session_id":"remote_1"}`),
		IdempotencyKey: "responder:evidence:1",
	}
	if err := command.Validate(); err != nil {
		t.Fatalf("get_session_evidence rejected by the protocol: %v", err)
	}
	encoded, err := json.Marshal(sampleSessionEvidence())
	if err != nil {
		t.Fatal(err)
	}
	result := CommandResult{CommandID: "command-1", State: "succeeded", OperationKey: "responder:evidence:1", Resource: encoded}
	if err := result.Validate(); err != nil {
		t.Fatalf("an evidence result was refused: %v", err)
	}
}
