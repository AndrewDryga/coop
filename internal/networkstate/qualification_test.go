package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

const smokeDomain = "example.com"

func qualifiedClientsFixture() []QualifiedClient {
	return []QualifiedClient{
		{Provider: "claude", Client: egress.ClientCLI, Version: "2.1.260"},
		{Provider: "claude", Client: egress.ClientACP, Version: "0.75.1"},
	}
}

func qualificationExecutionSpec(t *testing.T, smoke *QualificationSmoke) ExecutionSpec {
	t.Helper()
	mode := egress.Filtered
	project := t.TempDir()
	policy, err := smoke.store.Admit(project, Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	spec := smoke.candidate
	return ExecutionSpec{Project: project, PolicyFingerprint: policy.Fingerprint, Runtime: "docker",
		DaemonID: spec.Runtime.DaemonID, Endpoint: spec.Runtime.Endpoint, ClientImage: spec.ClientImage, GatewayImage: spec.GatewayImage}
}

// This is a deliberately synthetic storage fixture, not runtime qualification.
// Only a real host preflight may establish these causal facts for an actual image.
func qualificationFixture(t *testing.T) (*QualificationSmoke, ExecutionSpec) {
	t.Helper()
	smoke := executionSmoke(t, openStore(t))
	spec := qualificationExecutionSpec(t, smoke)
	r, err := smoke.CreateExecution(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	r, err = smoke.store.mutateExecution(context.Background(), r.ID, r.Revision, func(r *Execution) (bool, error) {
		for i := range r.Resources {
			resource := &r.Resources[i]
			resource.State = "gone"
			if resource.Kind == "volume" {
				resource.ID = resource.Name
			} else {
				resource.ID = strings.Repeat("a", 64)
			}
		}
		r.Artifact.State = "gone"
		r.Snapshot.Terminal, r.WorkloadStarted, r.ObserverAfterWorkload = true, true, true
		r.ReadySequence, r.Snapshot.Sequence = 1, 1
		exact := networkview.MetricCoverage{Status: "exact"}
		r.Snapshot.Coverage = networkview.Coverage{ProxyBytes: exact, Connections: exact, UpstreamFailures: exact, KernelPackets: exact,
			GuardDenials: exact, MaintenanceQueries: exact, MaintenanceBytes: exact, SocketInventory: exact, BoundaryAttribution: exact}
		setSmokeObservationFixture(&r.Snapshot)
		return sealExecution(r, "exited", false)
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := QualificationEvidenceBytes(r.Receipt.Snapshot, map[string]string{"fixture": "synthetic-unit-only", "run_id": r.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := smoke.RecordEvidence(r.ID, data); err != nil {
		t.Fatal(err)
	}
	return smoke, spec
}

func executionSmoke(t *testing.T, s *Store) *QualificationSmoke {
	t.Helper()
	spec := candidateFixture()
	spec.ClientImage, spec.GatewayImage = spec.GatewayImage, spec.ClientImage
	smoke, err := s.BeginQualification(spec, qualifiedClientsFixture())
	if err != nil {
		t.Fatal(err)
	}
	return smoke
}

func setSmokeObservationFixture(s *networkview.Snapshot) {
	s.AsOf = time.Now().UTC()
	s.Scope, s.Availability = "proxy-streams-and-sampled-tcp-sockets", "available"
	s.Health = networkview.HealthLayers{Enforcer: networkview.Health{Status: "ready"}, Gateway: networkview.Health{Status: "stopped"},
		Resolver: networkview.Health{Status: "ready"}, Collector: networkview.Health{Status: "ready"}}
	s.LiveConnections, s.UnknownConnections, s.KernelClosingSockets = networkview.Value(0), networkview.Value(0), networkview.Value(0)
	s.Loss = networkview.Loss{OmittedDetails: networkview.Value(0)}
	s.Counters = &networkview.Counters{SentBytes: networkview.Value(0), ReceivedBytes: networkview.Value(0), Connections: networkview.Value(0),
		UpstreamFailures: networkview.Value(0), DeniedPackets: networkview.Value(0), ProtectedPackets: networkview.Value(0),
		DeniedDNSQueries: networkview.Value(0), DeniedTLS: networkview.Value(0), MaintenanceQueries: networkview.Value(0),
		MaintenanceFailures: networkview.Value(0), IngressDenials: networkview.Value(0), MaintenanceSentBytes: networkview.Value(0), MaintenanceReceivedBytes: networkview.Value(0)}
	s.Sources = []networkview.Source{{ID: "guard", ObservedAt: &s.AsOf, Status: "live"},
		{ID: "proxy", ObservedAt: &s.AsOf, Status: "stopped", Reason: "drained"}, {ID: "kernel", ObservedAt: &s.AsOf, Status: "live"},
		{ID: "socket-inventory", ObservedAt: &s.AsOf, Status: "sampled", Reason: "sampled-not-packet-correlated"}}
	s.Connections = []networkview.Connection{{ID: "c1", DestinationID: "d1", State: "closed", Transport: "tls", Name: smokeDomain,
		NameSource: "sni", RuleID: "r1", ObservedAt: s.AsOf, StartedAt: &s.AsOf, SentBytes: networkview.Value(517), ReceivedBytes: networkview.Value(1024)}}
	s.Denials = []networkview.Denial{{ID: "n1", Source: "guard", Sequence: 1, Basis: "observed", At: s.AsOf,
		Kind: "dns_denied", Reason: "unapproved_name", Name: "example.org"}}
}

func TestNetworkQualificationRequiresACompletedSmoke(t *testing.T) {
	smoke, spec := qualificationFixture(t)
	if got, err := smoke.store.CreateExecution(context.Background(), spec); err == nil || got.ID != "" {
		t.Fatal("candidate alone authorized a workload")
	}
	if got, err := smoke.CreateExecution(context.Background(), spec); err == nil || got.ID != "" {
		t.Fatal("one preflight ran a second smoke attempt")
	}
	q, err := smoke.Complete(smokeDomain)
	if err != nil {
		t.Fatal(err)
	}
	again, err := smoke.Complete(smokeDomain)
	if err != nil || again.ID != q.ID {
		t.Fatal("qualification publication is not idempotent", err)
	}
	canonical, err := canonicalQualifiedClients(qualifiedClientsFixture())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(q.Clients, canonical) || q.Smoke.RunID == "" || q.CompletedAt.IsZero() {
		t.Fatalf("record lost its client or smoke identity: %#v", q)
	}
	spec.QualificationID = q.ID
	r, err := smoke.store.CreateExecution(context.Background(), spec)
	if err != nil || r.Purpose != "workload" || r.ClientImage != spec.ClientImage {
		t.Fatal("qualified launch lost exact binding", err)
	}
	for _, change := range []func(*ExecutionSpec){
		func(s *ExecutionSpec) { s.ClientImage = "coop:latest" },
		func(s *ExecutionSpec) { s.GatewayImage = "sha256:" + strings.Repeat("f", 64) },
		func(s *ExecutionSpec) { s.DaemonID = "another-daemon" },
		func(s *ExecutionSpec) { s.Endpoint = "unix:///another.sock" },
	} {
		changed := spec
		change(&changed)
		if got, err := smoke.store.CreateExecution(context.Background(), changed); err == nil || got.ID != "" {
			t.Fatal("foreign image/runtime launch accepted")
		}
	}
	// Once sealed, the immutable summary survives execution/receipt expiry.
	if err := smoke.store.root.Remove("execution-" + q.Smoke.RunID + ".json"); err != nil {
		t.Fatal(err)
	}
	if got, err := smoke.store.Qualification(q.ID); err != nil || !equalJSON(got, q) {
		t.Fatal("receipt expiry erased the completed record", err)
	}
	listed, err := smoke.store.Qualifications(context.Background())
	if err != nil || len(listed) != 1 || listed[0].ID != q.ID {
		t.Fatal("published record is absent from the host index", err)
	}
}

func TestNetworkSmokeRejectsWrongOutcomeUnresolvedCustodyAndUnprovenTraffic(t *testing.T) {
	smoke, _ := qualificationFixture(t)
	r, err := smoke.store.Execution(smoke.runID)
	if err != nil {
		t.Fatal(err)
	}
	for reason, change := range map[string]func(*Execution){
		"wrong workload":       func(r *Execution) { r.Receipt.Workload = "cancelled" },
		"partial completeness": func(r *Execution) { r.Receipt.Completeness = "partial" },
		"live resource":        func(r *Execution) { r.Resources[0].State = "started" },
		"remaining files":      func(r *Execution) { r.Artifact.State = "planned" },
		"never started":        func(r *Execution) { r.WorkloadStarted = false },
		"never ready":          func(r *Execution) { r.ReadySequence = 0 },
		"inexact proxy bytes": func(r *Execution) {
			r.Receipt.Snapshot.Coverage.ProxyBytes = networkview.MetricCoverage{Status: "lower-bound", Reason: "collector_gap"}
		},
		"no allowed connection": func(r *Execution) { r.Receipt.Snapshot.Connections = nil },
		"unattributed connection": func(r *Execution) {
			r.Receipt.Snapshot.Connections[0].RuleID = ""
		},
		"another destination": func(r *Execution) { r.Receipt.Snapshot.Connections[0].Name = "other.example" },
		"no measured bytes":   func(r *Execution) { r.Receipt.Snapshot.Connections[0].ReceivedBytes = networkview.Value(0) },
		"no denial":           func(r *Execution) { r.Receipt.Snapshot.Denials = nil },
		"unobserved denial":   func(r *Execution) { r.Receipt.Snapshot.Denials[0].Basis = "inferred" },
	} {
		var changed Execution
		data, _ := json.Marshal(r)
		_ = json.Unmarshal(data, &changed)
		change(&changed)
		if err := validateSmokeOutcome(changed, smokeDomain); err == nil {
			t.Errorf("%s accepted", reason)
		}
	}
	if err := validateSmokeOutcome(r, smokeDomain); err != nil {
		t.Fatal("a clean smoke run was refused", err)
	}
}

func TestNetworkQualificationCoversOnlyTheClientsInItsImage(t *testing.T) {
	smoke, _ := qualificationFixture(t)
	q, err := smoke.Complete(smokeDomain)
	if err != nil {
		t.Fatal(err)
	}
	dependency := egress.Dependency{Provider: "claude", Client: egress.ClientCLI, Backend: "direct", AuthMode: "oauth-file", Version: "2026-09-09.1"}
	policy := egress.Snapshot{Mode: egress.Filtered, Dependencies: []egress.Dependency{dependency}}
	if err := q.RequireLaunch(policy); err != nil {
		t.Fatal("the image's own client was refused", err)
	}
	// The release's bundle metadata is the version claim; a per-host record
	// must not force requalification for a new endpoint release or auth variant.
	for _, change := range []func(*egress.Dependency){
		func(d *egress.Dependency) { d.Version = "2027-01-01.1" },
		func(d *egress.Dependency) { d.Backend = "bedrock" },
		func(d *egress.Dependency) { d.AuthMode = "api-key" },
	} {
		widened := dependency
		change(&widened)
		if err := q.RequireLaunch(egress.Snapshot{Mode: egress.Filtered, Dependencies: []egress.Dependency{widened}}); err != nil {
			t.Fatal("per-host record was mistaken for the release version claim", err)
		}
	}
	for _, absent := range []egress.Dependency{
		{Provider: "gemini", Client: egress.ClientCLI}, {Provider: "grok", Client: egress.ClientACP},
	} {
		if err := q.RequireLaunch(egress.Snapshot{Mode: egress.Filtered, Dependencies: []egress.Dependency{absent}}); err == nil {
			t.Fatal("a provider the image does not contain borrowed coverage")
		}
	}
	q.Contract = "another-contract"
	if err := q.RequireLaunch(policy); err == nil {
		t.Fatal("a record from another enforcement contract launched")
	}
}

func TestNetworkQualificationRejectsTamperAndFilesystemTraps(t *testing.T) {
	for _, kind := range []string{"content", "unknown-field", "noncanonical", "symlink", "fifo", "oversized", "public", "key-loss"} {
		t.Run(kind, func(t *testing.T) {
			smoke, _ := qualificationFixture(t)
			q, err := smoke.Complete(smokeDomain)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(smoke.store.Path(), "qualification-"+q.ID+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "content":
				q.Candidate.ClientImage = "sha256:" + strings.Repeat("f", 64)
				data, _ = json.Marshal(q)
			case "unknown-field":
				data = append([]byte(`{"passed":true,`), data[1:]...)
			case "noncanonical":
				slices.Reverse(q.Clients)
				data, _ = json.Marshal(q)
			case "symlink":
				if err := os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				data = []byte(strings.Repeat(" ", maxQualificationBytes+1))
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "key-loss":
				if err := smoke.store.root.Remove("owner.key"); err != nil {
					t.Fatal(err)
				}
			}
			if slices.Contains([]string{"content", "unknown-field", "noncanonical", "oversized"}, kind) {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := smoke.store.Qualification(q.ID); err == nil || got.ID != "" {
				t.Fatal("invalid qualification became authority")
			}
		})
	}
}

func TestNetworkQualificationPublicationConfirmsDurability(t *testing.T) {
	smoke, _ := qualificationFixture(t)
	wanted := errors.New("qualification directory fsync")
	smoke.store.syncDir = func(dir *os.File) error {
		entries, err := os.ReadDir(smoke.store.Path())
		if err != nil {
			return err
		}
		for _, entry := range entries {
			id := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "qualification-"), ".json")
			if strings.HasPrefix(entry.Name(), "qualification-") && lowerHex(id, 64) {
				return wanted
			}
		}
		return dir.Sync()
	}
	if q, err := smoke.Complete(smokeDomain); !errors.Is(err, wanted) || q.ID != "" {
		t.Fatal("ambiguous durability returned authority", err)
	}
	smoke.store.syncDir = nil
	if q, err := smoke.Complete(smokeDomain); err != nil || q.ID == "" {
		t.Fatal("retry did not confirm existing publication", err)
	}
}

func TestNetworkQualificationSmokeIsAPrivateHostCapability(t *testing.T) {
	s := openStore(t)
	smoke := executionSmoke(t, s)
	// A containing DTO must not accidentally serialize the host capability.
	data, _ := json.Marshal(struct {
		Smoke *QualificationSmoke `json:"smoke"`
	}{Smoke: smoke})
	if string(data) != `{"smoke":{}}` {
		t.Fatal("preflight capability serialized")
	}
	var empty QualificationSmoke
	if _, err := empty.CreateExecution(context.Background(), ExecutionSpec{}); err == nil {
		t.Fatal("zero capability authorized a smoke run")
	}
	if _, err := empty.Complete(smokeDomain); err == nil {
		t.Fatal("zero capability published a record")
	}
	// A crashed setup is rerun, never resumed: nothing recovers a preflight.
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	for _, value := range []any{s, evidence} {
		if _, exists := reflect.TypeOf(value).MethodByName("RecoverQualification"); exists {
			t.Fatal("a departed preflight can be resumed")
		}
	}
}

func TestQualifiedClientsRejectInvalidOrRepeatedBuilds(t *testing.T) {
	s := openStore(t)
	for name, clients := range map[string][]QualifiedClient{
		"empty":          nil,
		"unknown client": {{Provider: "claude", Client: "sdk", Version: "1.0.0"}},
		"no provider":    {{Client: egress.ClientCLI, Version: "1.0.0"}},
		"no version":     {{Provider: "claude", Client: egress.ClientCLI}},
		"duplicate": {{Provider: "claude", Client: egress.ClientCLI, Version: "1.0.0"},
			{Provider: "claude", Client: egress.ClientCLI, Version: "1.0.0"}},
	} {
		if _, err := s.BeginQualification(candidateFixture(), clients); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
