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

func qualificationExecutionSpec(t *testing.T, trial *QualificationTrial) ExecutionSpec {
	t.Helper()
	mode := egress.Filtered
	project := t.TempDir()
	policy, err := trial.store.Admit(project, Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	spec := trial.candidate
	return ExecutionSpec{Project: project, PolicyFingerprint: policy.Fingerprint, Runtime: "docker",
		DaemonID: spec.Runtime.DaemonID, Endpoint: spec.Runtime.Endpoint, ClientImage: spec.ClientImage, GatewayImage: spec.GatewayImage}
}

// These are deliberately synthetic storage fixtures, not runtime qualification.
// Only a real host harness may establish these causal facts for an actual image.
func qualificationProofFixture(t *testing.T, trial *QualificationTrial, spec ExecutionSpec, name string, client *QualifiedClient, observe ...func(*networkview.Snapshot)) QualificationProof {
	t.Helper()
	r, err := trial.CreateExecution(context.Background(), spec, name, client)
	if err != nil {
		t.Fatal(err)
	}
	r, err = trial.store.mutateExecution(context.Background(), r.ID, r.Revision, func(r *Execution) (bool, error) {
		for i := range r.Resources {
			resource := &r.Resources[i]
			resource.State = "gone"
			if resource.Kind == "volume" {
				resource.ID = resource.Name
			} else if name != "init-failure" || resource.Role != "agent" {
				resource.ID = strings.Repeat("a", 64)
			}
		}
		r.Artifact.State = "gone"
		r.Snapshot.Terminal = true
		r.WorkloadStarted, r.ObserverAfterWorkload = name != "init-failure", true
		if r.WorkloadStarted {
			r.ReadySequence = 1
			r.Snapshot.Sequence = 1
			exact := networkview.MetricCoverage{Status: "exact"}
			r.Snapshot.Coverage = networkview.Coverage{ProxyBytes: exact, Connections: exact, UpstreamFailures: exact, KernelPackets: exact,
				GuardDenials: exact, MaintenanceQueries: exact, MaintenanceBytes: exact, SocketInventory: exact, BoundaryAttribution: exact}
			setQualificationObservationFixture(&r.Snapshot)
		}
		workload := "exited"
		switch name {
		case "guard-loss":
			workload = "runtime_failed"
		case "init-failure":
			workload = "launch_failed"
		case "recovery":
			workload = "supervisor_lost"
		case "cancel":
			workload = "cancelled"
		}
		if slices.Contains([]string{"guard-loss", "collector-loss", "init-failure", "recovery"}, name) {
			r.Snapshot.Terminal, r.ObserverAfterWorkload = false, false
		}
		for _, change := range observe {
			change(&r.Snapshot)
		}
		return sealExecution(r, workload, name == "recovery")
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := qualificationEvidenceBytes(r.Receipt.Snapshot, map[string]string{"fixture": "synthetic-unit-only", "case": name, "run_id": r.ID})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := trial.RecordEvidence(r.ID, data)
	if err != nil {
		t.Fatal(err)
	}
	return QualificationProof{Case: name, RunID: r.ID, Epoch: r.Epoch, ReceiptDigest: r.Receipt.Digest,
		EvidenceDigest: digest, Client: client}
}

func qualificationFixture(t *testing.T, clients []QualifiedClient) (*QualificationTrial, ExecutionSpec, []QualificationProof) {
	t.Helper()
	trial := executionTrial(t, openStore(t))
	spec := qualificationExecutionSpec(t, trial)
	if len(clients) != 0 {
		mode := egress.Filtered
		input := Admission{InvocationMode: &mode}
		for _, client := range clients {
			d := client.Dependency
			bundle := egress.Bundle{Provider: d.Provider, Client: d.Client, Backend: d.Backend, AuthMode: d.AuthMode, Version: d.Version,
				Core: []egress.Rule{{To: egress.Destination{Domain: "api.example.com"}, Protocol: "tls", Ports: []int{443}}}, Features: map[string][]egress.Rule{}}
			for _, feature := range client.Features {
				bundle.Features[feature] = []egress.Rule{{To: egress.Destination{Domain: "feature.example.com"}, Protocol: "tls", Ports: []int{443}}}
			}
			input.Bundles = append(input.Bundles, bundle)
			if len(client.Features) != 0 {
				input.Operator = append(input.Operator, egress.Input{Origin: egress.Origin{Kind: "operator"}, Rules: []egress.Rule{{To: egress.Destination{Provider: d.Provider, Features: client.Features}}}})
			}
		}
		policy, err := trial.store.Admit(spec.Project, input)
		if err != nil {
			t.Fatal(err)
		}
		spec.PolicyFingerprint = policy.Fingerprint
	}
	var proofs []QualificationProof
	for _, name := range []string{"enforcement", "guard-loss", "collector-loss", "cancel", "init-failure", "recovery", "concurrency", "observation-baseline", "short-flow"} {
		proofs = append(proofs, qualificationProofFixture(t, trial, spec, name, nil))
	}
	for _, client := range clients {
		startID := ""
		for _, name := range []string{"provider-start", "provider-resume"} {
			proof := qualificationProofFixture(t, trial, spec, name, &client)
			if name == "provider-start" {
				startID = proof.RunID
			} else {
				proof.ResumesRunID = startID
			}
			proofs = append(proofs, proof)
		}
		if client.MCPProjection != "none" {
			proofs = append(proofs, qualificationProofFixture(t, trial, spec, "mcp", &client))
		}
	}
	return trial, spec, proofs
}

func setQualificationObservationFixture(s *networkview.Snapshot) {
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
}

func TestNetworkQualificationRequiresCompletedBoundTrials(t *testing.T) {
	trial, spec, proofs := qualificationFixture(t, nil)
	if got, err := trial.store.CreateExecution(context.Background(), spec); err == nil || got.ID != "" {
		t.Fatal("candidate alone authorized a workload")
	}
	for name, change := range map[string]func([]QualificationProof) []QualificationProof{
		"missing case": func(p []QualificationProof) []QualificationProof { return p[1:] },
		"wrong epoch":  func(p []QualificationProof) []QualificationProof { p[0].Epoch = strings.Repeat("f", 32); return p },
		"wrong receipt": func(p []QualificationProof) []QualificationProof {
			p[0].ReceiptDigest = strings.Repeat("f", 64)
			return p
		},
		"generic failed curl":  func(p []QualificationProof) []QualificationProof { p[0].Case = "curl-failed"; return p },
		"missing observations": func(p []QualificationProof) []QualificationProof { p[0].EvidenceDigest = ""; return p },
		"duplicate run":        func(p []QualificationProof) []QualificationProof { p[1].RunID = p[0].RunID; return p },
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := trial.Complete(nil, change(slices.Clone(proofs))); err == nil || got.ID != "" {
				t.Fatal("invalid proof published qualification", err)
			}
		})
	}
	q, err := trial.Complete(nil, proofs)
	if err != nil {
		t.Fatal(err)
	}
	again, err := trial.Complete(nil, proofs)
	if err != nil || again.ID != q.ID {
		t.Fatal("qualification publication is not idempotent", err)
	}
	spec.QualificationID = q.ID
	r, err := trial.store.CreateExecution(context.Background(), spec)
	if err != nil || r.Purpose != "workload" || r.ClientImage != spec.ClientImage || r.TrialGroup != "" {
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
		if got, err := trial.store.CreateExecution(context.Background(), changed); err == nil || got.ID != "" {
			t.Fatal("foreign image/runtime launch accepted")
		}
	}
	// Once sealed, immutable summaries survive original execution/receipt expiry.
	for _, proof := range proofs {
		if err := trial.store.root.Remove("execution-" + proof.RunID + ".json"); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := trial.store.Qualification(q.ID); err != nil || !equalJSON(got, q) {
		t.Fatal("receipt expiry erased completed qualification", err)
	}
}

func TestNetworkQualificationRejectsWrongOutcomeAndUnresolvedCustody(t *testing.T) {
	for _, name := range []string{"enforcement", "guard-loss", "collector-loss", "cancel", "init-failure", "recovery", "concurrency"} {
		t.Run(name, func(t *testing.T) {
			trial, _, proofs := qualificationFixture(t, nil)
			proof := proofs[slices.IndexFunc(proofs, func(p QualificationProof) bool { return p.Case == name })]
			r, err := trial.store.Execution(proof.RunID)
			if err != nil {
				t.Fatal(err)
			}
			for reason, change := range map[string]func(*Execution){
				"wrong workload":       func(r *Execution) { r.Receipt.Workload = "not-an-outcome" },
				"unknown completeness": func(r *Execution) { r.Receipt.Completeness = "unknown" },
				"unrelated loss": func(r *Execution) {
					r.Receipt.Snapshot.Loss.Reasons = []string{"unrelated"}
					r.Receipt.Completeness = "partial"
				},
				"live resource":   func(r *Execution) { r.Resources[0].State = "started" },
				"remaining files": func(r *Execution) { r.Artifact.State = "planned" },
			} {
				var changed Execution
				data, _ := json.Marshal(r)
				_ = json.Unmarshal(data, &changed)
				change(&changed)
				if err := validateQualificationOutcome(changed); err == nil {
					t.Errorf("%s accepted", reason)
				}
			}
			if name != "init-failure" {
				r.WorkloadStarted = false
				if err := validateQualificationOutcome(r); err == nil {
					t.Fatal("container creation was mistaken for agent startup")
				}
			}
		})
	}
}

func TestNetworkQualificationClientCoverageIsExactAndSeparateFromGrants(t *testing.T) {
	client := QualifiedClient{Dependency: egress.Dependency{Provider: "anthropic", Client: egress.ClientCLI, Backend: "direct", AuthMode: "api-key", Version: "2026-09-08"}, Features: []string{"cloud-mcp"}, MCPProjection: "none"}
	trial, _, proofs := qualificationFixture(t, []QualifiedClient{client})
	if _, err := trial.Complete([]QualifiedClient{client}, proofs[:len(proofs)-1]); err == nil {
		t.Fatal("missing authenticated resume accepted")
	}
	q, err := trial.Complete([]QualifiedClient{client}, proofs)
	if err != nil {
		t.Fatal(err)
	}
	policy := egress.Snapshot{Mode: egress.Filtered, Dependencies: []egress.Dependency{client.Dependency}}
	if err := q.requirePolicy(policy, policy.Dependencies, nil); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*egress.Dependency){
		func(d *egress.Dependency) { d.Client = egress.ClientACP },
		func(d *egress.Dependency) { d.Backend = "bedrock" },
		func(d *egress.Dependency) { d.AuthMode = "oauth" },
		func(d *egress.Dependency) { d.Version = "next" },
	} {
		dependency := client.Dependency
		change(&dependency)
		policy := egress.Snapshot{Mode: egress.Filtered, Dependencies: []egress.Dependency{dependency}}
		if err := q.requirePolicy(policy, policy.Dependencies, nil); err == nil {
			t.Fatal("another provider variant borrowed coverage")
		}
	}
	rule := egress.Rule{To: egress.Destination{Domain: "extra.example.com"}, Protocol: "tls", Ports: []int{443}}
	policy.Grants = []egress.Grant{{Rule: rule, Origins: []egress.Origin{{Kind: "owner", Provider: client.Dependency.Provider, Client: client.Dependency.Client, Backend: client.Dependency.Backend,
		AuthMode: client.Dependency.AuthMode, BundleVersion: client.Dependency.Version, Feature: "other-feature"}}}}
	if err := q.requirePolicy(policy, policy.Dependencies, nil); err == nil {
		t.Fatal("unqualified optional feature accepted")
	}
	policy.Grants = []egress.Grant{{Rule: rule, Origins: []egress.Origin{{Kind: "owner", Name: "extra.example"}}}}
	if err := q.requirePolicy(policy, policy.Dependencies, nil); err != nil {
		t.Fatal("explicit destination falsely changed provider coverage", err)
	}
}

func TestNetworkQualificationCannotSpliceFeaturesAndMCPFromDifferentTrials(t *testing.T) {
	dependency := egress.Dependency{Provider: "claude", Client: egress.ClientCLI, Backend: "direct", AuthMode: "oauth-file", Version: "fixture"}
	projection := strings.Repeat("a", 64)
	policy := egress.Snapshot{Mode: egress.Filtered, Dependencies: []egress.Dependency{dependency}, Grants: []egress.Grant{{
		Rule:    egress.Rule{To: egress.Destination{Domain: "mcp.example.com"}, Protocol: "tls", Ports: []int{443}},
		Origins: []egress.Origin{{Kind: "owner", Provider: dependency.Provider, Client: dependency.Client, Backend: dependency.Backend, AuthMode: dependency.AuthMode, BundleVersion: dependency.Version, Feature: "cloud-mcp"}},
	}}}
	q := Qualification{Coverage: []QualifiedClient{
		{Dependency: dependency, Features: []string{"cloud-mcp"}, MCPProjection: "none"},
		{Dependency: dependency, MCPProjection: projection},
	}}
	if err := q.RequireLaunch(policy, projection); err == nil {
		t.Fatal("separate feature and MCP proofs were spliced into untested coverage")
	}
	q.Coverage = append(q.Coverage, QualifiedClient{Dependency: dependency, Features: []string{"cloud-mcp"}, MCPProjection: projection})
	if err := q.RequireLaunch(policy, projection); err != nil {
		t.Fatal("exact tested combination refused", err)
	}
	if err := q.RequireLaunch(policy, "malformed"); err == nil {
		t.Fatal("malformed projection accepted")
	}
}

func TestNetworkQualificationRejectsTamperAndFilesystemTraps(t *testing.T) {
	for _, kind := range []string{"content", "unknown-field", "noncanonical", "symlink", "fifo", "oversized", "public", "key-loss"} {
		t.Run(kind, func(t *testing.T) {
			trial, _, proofs := qualificationFixture(t, nil)
			q, err := trial.Complete(nil, proofs)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(trial.store.Path(), "qualification-"+q.ID+".json")
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
				slices.Reverse(q.Proofs)
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
				if err := trial.store.root.Remove("owner.key"); err != nil {
					t.Fatal(err)
				}
			}
			if slices.Contains([]string{"content", "unknown-field", "noncanonical", "oversized"}, kind) {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := trial.store.Qualification(q.ID); err == nil || got.ID != "" {
				t.Fatal("invalid qualification became authority")
			}
		})
	}
}

func TestNetworkQualificationPublicationConfirmsDurability(t *testing.T) {
	trial, _, proofs := qualificationFixture(t, nil)
	wanted := errors.New("qualification directory fsync")
	trial.store.syncDir = func(dir *os.File) error {
		entries, err := os.ReadDir(trial.store.Path())
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
	if q, err := trial.Complete(nil, proofs); !errors.Is(err, wanted) || q.ID != "" {
		t.Fatal("ambiguous durability returned authority", err)
	}
	trial.store.syncDir = nil
	if q, err := trial.Complete(nil, proofs); err != nil || q.ID == "" {
		t.Fatal("retry did not confirm existing publication", err)
	}
}

func TestNetworkQualificationTrialIsAPrivateHostCapability(t *testing.T) {
	s := openStore(t)
	trial := executionTrial(t, s)
	// A containing DTO must not accidentally serialize the host capability.
	data, _ := json.Marshal(struct {
		Trial *QualificationTrial `json:"trial"`
	}{Trial: trial})
	if string(data) != `{"trial":{}}` {
		t.Fatal("trial capability serialized")
	}
	var empty QualificationTrial
	if _, err := empty.CreateExecution(context.Background(), ExecutionSpec{}, "enforcement", nil); err == nil {
		t.Fatal("zero capability authorized trial")
	}
	// A crashed setup is rerun, never resumed: nothing recovers a trial group.
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	for _, value := range []any{s, evidence} {
		if _, exists := reflect.TypeOf(value).MethodByName("RecoverQualification"); exists {
			t.Fatal("a departed trial can be resumed")
		}
	}
}
