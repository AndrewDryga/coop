package networkstate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/processidentity"
)

// QualificationContract changes when the host qualification harness changes its
// enforcement or functional acceptance contract. Endpoint bundle releases and
// installed client closures have their own independent identities.
const QualificationContract = "visible-sni-tls443-v2"

const maxQualificationBytes = 256 << 10

var errObsoleteQualification = errors.New("obsolete network qualification requires explicit requalification")

// QualifiedClient is functional coverage, not a grant. MCPProjection identifies
// the captured transport/routing shape without access tokens or account names.
// "none" is explicit; omission cannot mean that arbitrary MCP is supported.
type QualifiedClient struct {
	Dependency    egress.Dependency `json:"dependency"`
	Features      []string          `json:"features"`
	MCPProjection string            `json:"mcp_projection"`
}

// QualificationProof is supplied by the trusted host harness after checking its
// fixed case. EvidenceDigest covers a retained bounded host observation artifact,
// never agent output or a requester's passed:true. Storage verifies its bytes and
// run custody; the harness owns protocol and fault-causality interpretation.
type QualificationProof struct {
	Case           string                    `json:"case"`
	RunID          string                    `json:"run_id"`
	Epoch          string                    `json:"epoch"`
	InputsID       string                    `json:"inputs_id"`
	ReceiptDigest  string                    `json:"receipt_digest"`
	EvidenceDigest string                    `json:"evidence_digest"`
	ResumesRunID   string                    `json:"resumes_run_id,omitempty"`
	Client         *QualifiedClient          `json:"client,omitempty"`
	Observation    *QualificationObservation `json:"observation,omitempty"`
}

type Qualification struct {
	Version     int                  `json:"version"`
	ID          string               `json:"id"`
	CandidateID string               `json:"candidate_id"`
	Contract    string               `json:"contract"`
	TrialGroup  string               `json:"trial_group"`
	CompletedAt time.Time            `json:"completed_at"`
	Coverage    []QualifiedClient    `json:"coverage"`
	Proofs      []QualificationProof `json:"proofs"`
}

// QualificationTrial is a host-only capability. Its zero value and serialized
// form confer no authority. It cannot be reconstructed from a worker request or
// used to restart an old trial epoch. Host recovery may resume a departed trial
// group, but every new case still receives a fresh execution and epoch.
type QualificationTrial struct {
	store     *Store
	candidate Candidate
	group     string
}

func (t *QualificationTrial) CandidateID() string {
	if t == nil {
		return ""
	}
	return t.candidate.ID
}

func (s *Store) BeginQualification(candidateID string) (*QualificationTrial, error) {
	candidate, err := s.Candidate(candidateID)
	if err != nil {
		return nil, err
	}
	group, err := randomExecutionID()
	if err != nil {
		return nil, err
	}
	return &QualificationTrial{store: s, candidate: candidate, group: group}, nil
}

// RecoverQualification is the explicit host recovery seam used after a real
// child-supervisor crash. It never rewrites ownership or restarts its execution.
// Evidence-only handles and remote requests cannot obtain this capability.
func (s *Store) RecoverQualification(anchorRunID string) (*QualificationTrial, error) {
	record, err := s.Execution(anchorRunID)
	if err != nil {
		return nil, err
	}
	state := processidentity.Inspect(record.Supervisor.PID, record.Supervisor.StartToken)
	if record.Version != ExecutionVersion || record.Purpose != "qualification" || record.QualificationContract != QualificationContract ||
		(state != processidentity.Gone && state != processidentity.Mismatch) {
		return nil, errors.New("qualification recovery requires a provably departed trial supervisor")
	}
	candidate, err := s.Candidate(record.CandidateID)
	if err != nil {
		return nil, err
	}
	return &QualificationTrial{store: s, candidate: candidate, group: record.TrialGroup}, nil
}

func (t *QualificationTrial) intact() error {
	if t == nil || t.store == nil || !lowerHex(t.group, 32) {
		return errors.New("qualification requires a private host trial")
	}
	candidate, err := t.store.Candidate(t.candidate.ID)
	if err != nil {
		return err
	}
	if !equalJSON(candidate, t.candidate) {
		return errors.New("qualification candidate is unavailable or changed")
	}
	return nil
}

func validQualificationCase(name string) bool {
	return slices.Contains([]string{"enforcement", "guard-loss", "collector-loss", "cancel", "init-failure", "recovery", "concurrency", "observation-baseline", "short-flow", "provider-start", "provider-resume", "mcp"}, name)
}

func canonicalQualifiedClient(client QualifiedClient) (QualifiedClient, error) {
	d := client.Dependency
	_, err := egress.SelectedBundles([]egress.Bundle{{Provider: d.Provider, Client: d.Client, Version: d.Version, Backend: d.Backend, AuthMode: d.AuthMode}})
	if err != nil || len(client.Features) > egress.MaxConstraints || client.MCPProjection != "none" && !lowerHex(client.MCPProjection, 64) {
		return QualifiedClient{}, errors.New("invalid network qualification client coverage")
	}
	client.Features = append([]string{}, client.Features...)
	for _, feature := range client.Features {
		if !safeRecordToken(feature, 63) || strings.Trim(feature, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || strings.HasPrefix(feature, "-") || strings.HasSuffix(feature, "-") {
			return QualifiedClient{}, errors.New("invalid qualified provider feature")
		}
	}
	slices.Sort(client.Features)
	client.Features = slices.Compact(client.Features)
	return client, nil
}

func qualificationKey(value any) string { data, _ := json.Marshal(value); return string(data) }

// QualificationCoverage validates the explicit host plan before running any
// canary. The aggregate bound leaves room for its repeated per-case descriptors
// and fixed proof references in the final immutable record.
func QualificationCoverage(clients []QualifiedClient) ([]QualifiedClient, error) {
	if len(clients) > 32 {
		return nil, errors.New("too many network qualification client variants")
	}
	clients = append([]QualifiedClient{}, clients...)
	for i, client := range clients {
		var err error
		clients[i], err = canonicalQualifiedClient(client)
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(clients, func(a, b QualifiedClient) int { return strings.Compare(qualificationKey(a), qualificationKey(b)) })
	for i := 1; i < len(clients); i++ {
		if equalJSON(clients[i-1], clients[i]) {
			return nil, errors.New("duplicate qualified client coverage")
		}
	}
	data, err := json.Marshal(clients)
	if err != nil || len(data) > 8<<10 {
		return nil, errors.New("network qualification client plan exceeds its byte bound")
	}
	return clients, nil
}

func canonicalQualification(q Qualification) (Qualification, error) {
	return canonicalQualificationFrame(q, true, false)
}

// The legacy branch is solely for authenticating obsolete records during
// discovery. It can never return launch authority through Qualification.
func canonicalQualificationFrame(q Qualification, observations, legacy bool) (Qualification, error) {
	version, contract := 2, QualificationContract
	if legacy {
		version, contract = 1, "visible-sni-tls443-v1"
	}
	if q.Version != version || q.Contract != contract || !lowerHex(q.CandidateID, 64) || !lowerHex(q.TrialGroup, 32) ||
		q.CompletedAt.IsZero() || len(q.Proofs) > 128 {
		return Qualification{}, errors.New("invalid network qualification identity or bounds")
	}
	var err error
	q.Coverage, err = QualificationCoverage(q.Coverage)
	if err != nil {
		return Qualification{}, err
	}
	needed := map[string]bool{}
	for _, name := range []string{"enforcement", "guard-loss", "collector-loss", "cancel", "init-failure", "recovery", "concurrency"} {
		needed[name] = true
	}
	if !legacy {
		needed["observation-baseline"], needed["short-flow"] = true, true
	}
	for _, client := range q.Coverage {
		for _, name := range []string{"provider-start", "provider-resume"} {
			needed[name+qualificationKey(client)] = true
		}
		if client.MCPProjection != "none" {
			needed["mcp"+qualificationKey(client)] = true
		}
	}
	q.Proofs = append([]QualificationProof{}, q.Proofs...)
	runs := map[string]bool{}
	for i, proof := range q.Proofs {
		if !validQualificationCase(proof.Case) || !lowerHex(proof.RunID, 32) || !lowerHex(proof.Epoch, 32) || !lowerHex(proof.InputsID, 64) ||
			!lowerHex(proof.ReceiptDigest, 64) || !lowerHex(proof.EvidenceDigest, 64) || runs[proof.RunID] ||
			(proof.Case == "provider-resume" && !lowerHex(proof.ResumesRunID, 32)) || (proof.Case != "provider-resume" && proof.ResumesRunID != "") {
			return Qualification{}, errors.New("invalid or repeated qualification proof")
		}
		runs[proof.RunID] = true
		key := proof.Case
		if proof.Client != nil {
			client, err := canonicalQualifiedClient(*proof.Client)
			if err != nil {
				return Qualification{}, err
			}
			proof.Client = &client
			key += qualificationKey(client)
		}
		if !needed[key] {
			return Qualification{}, errors.New("unexpected or duplicate qualification case")
		}
		delete(needed, key)
		if legacy && proof.Observation != nil {
			return Qualification{}, errors.New("obsolete qualification contains unsupported observations")
		}
		if observations || proof.Observation != nil {
			if err := validateQualificationObservation(proof.Observation); err != nil {
				return Qualification{}, err
			}
		}
		q.Proofs[i] = proof
	}
	if len(needed) != 0 {
		return Qualification{}, errors.New("qualification lacks mandatory runtime or selected client cases")
	}
	slices.SortFunc(q.Proofs, func(a, b QualificationProof) int { return strings.Compare(qualificationKey(a), qualificationKey(b)) })
	return q, nil
}

func (s *Store) qualificationID(q Qualification) string {
	q.ID = ""
	data, _ := json.Marshal(q)
	mac := hmac.New(sha256.New, s.key)
	prefix := "network-qualification-v2\x00"
	if q.Version == 1 {
		prefix = "network-qualification-v1\x00"
	}
	_, _ = mac.Write([]byte(prefix))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// Complete validates sealed trial receipts and current cleanup custody before
// publishing immutable proof summaries. Subsequent qualification reads do not
// depend on retained traffic detail or receipts that may legitimately expire.
func (t *QualificationTrial) Complete(coverage []QualifiedClient, proofs []QualificationProof) (Qualification, error) {
	if err := t.intact(); err != nil {
		return Qualification{}, err
	}
	q := Qualification{Version: 2, CandidateID: t.candidate.ID, Contract: QualificationContract, TrialGroup: t.group,
		CompletedAt: time.Unix(1, 0).UTC(), Coverage: coverage, Proofs: proofs}
	q, err := canonicalQualificationFrame(q, false, false)
	if err != nil {
		return Qualification{}, err
	}
	for i, proof := range q.Proofs {
		r, err := t.store.Execution(proof.RunID)
		if err != nil || r.Purpose != "qualification" || r.QualificationContract != QualificationContract || r.TrialGroup != t.group || r.TrialCase != proof.Case ||
			r.CandidateID != t.candidate.ID || r.Epoch != proof.Epoch || r.InputsID != proof.InputsID || !equalJSON(r.TrialClient, proof.Client) ||
			r.ClientImage != t.candidate.Spec.ClientImage || r.GatewayImage != t.candidate.Spec.GatewayImage ||
			r.DaemonID != t.candidate.Spec.Runtime.DaemonID || r.Endpoint != t.candidate.Spec.Runtime.Endpoint ||
			r.Receipt == nil || r.Receipt.Digest != proof.ReceiptDigest {
			return Qualification{}, errors.New("qualification proof does not match a sealed trial execution")
		}
		if err := validateQualificationOutcome(r); err != nil {
			return Qualification{}, err
		}
		claim, err := t.store.read(t.caseFile(proof.Case, proof.Client), 32)
		if err != nil || string(claim) != proof.RunID {
			return Qualification{}, errors.New("qualification proof is not the unique reserved case attempt")
		}
		evidence, err := t.store.read("qualification-evidence-"+proof.RunID+".json", MaxQualificationEvidenceBytes)
		digest := sha256.Sum256(evidence)
		if err != nil || hex.EncodeToString(digest[:]) != proof.EvidenceDigest {
			return Qualification{}, errors.New("qualification observation artifact is unavailable or changed")
		}
		if err := validateQualificationEvidence(evidence, r.Receipt); err != nil {
			return Qualification{}, err
		}
		observation, err := qualificationObservation(r.Receipt)
		if err != nil {
			return Qualification{}, err
		}
		if proof.Observation != nil && !equalJSON(proof.Observation, observation) {
			return Qualification{}, errors.New("supplied qualification observation differs from its sealed receipt")
		}
		q.Proofs[i].Observation = observation
		if proof.Client != nil && !slices.Contains(r.BundleReferences, dependencyReference(proof.Client.Dependency)) {
			return Qualification{}, errors.New("trial policy did not contain the claimed provider dependency")
		}
		if proof.Case == "provider-resume" && !slices.ContainsFunc(q.Proofs, func(start QualificationProof) bool {
			return start.Case == "provider-start" && start.RunID == proof.ResumesRunID && start.InputsID == proof.InputsID && equalJSON(start.Client, proof.Client)
		}) {
			return Qualification{}, errors.New("provider resume does not reference its exact qualified startup")
		}
		if r.Receipt.EndedAt.After(q.CompletedAt) {
			q.CompletedAt = *r.Receipt.EndedAt
		}
	}
	q, err = canonicalQualification(q)
	if err != nil {
		return Qualification{}, err
	}
	q.ID = t.store.qualificationID(q)
	data, err := json.Marshal(q)
	if err != nil || len(data) > maxQualificationBytes {
		return Qualification{}, errors.New("network qualification exceeds its byte bound")
	}
	name := "qualification-" + q.ID + ".json"
	if err := t.store.publish(name, data, false); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return Qualification{}, err
		}
		prior, err := t.store.read(name, maxQualificationBytes)
		if err != nil || !bytes.Equal(prior, data) {
			return Qualification{}, errors.New("network qualification identity collision or invalid content")
		}
	}
	if err := t.store.confirmPublication(); err != nil {
		return Qualification{}, err
	}
	if err := t.intact(); err != nil {
		return Qualification{}, err
	}
	return q, nil
}

func validateQualificationOutcome(r Execution) error {
	if r.Receipt == nil || r.Receipt.EndedAt == nil || r.Receipt.Finality != "final" {
		return errors.New("qualification requires a final sealed receipt")
	}
	for _, resource := range r.Resources {
		if resource.State != "gone" {
			return errors.New("qualification still owns unresolved runtime resources")
		}
	}
	if r.LaunchConfig.State != "gone" || r.RunFiles.State != "gone" {
		return errors.New("qualification still owns unresolved launch files")
	}
	workload, completeness := "exited", "complete"
	var reasons []string
	switch r.TrialCase {
	case "guard-loss":
		workload, completeness = "runtime_failed", "partial"
		reasons = []string{"observer_end_after_workload_unproven", "terminal_observation_unavailable"}
	case "collector-loss":
		completeness = "partial"
		reasons = []string{"terminal_observation_unavailable"}
	case "init-failure":
		workload, completeness = "launch_failed", "partial"
		reasons = []string{"observer_end_after_workload_unproven", "terminal_observation_unavailable"}
	case "recovery":
		workload, completeness = "supervisor_lost", "partial"
		reasons = []string{"terminal_observation_unavailable"}
	case "cancel":
		workload = "cancelled"
	}
	if r.Receipt.Workload != workload || completeness == "partial" && r.Receipt.Completeness != completeness ||
		completeness == "partial" && !r.Receipt.Snapshot.Loss.Unknown ||
		slices.ContainsFunc(reasons, func(reason string) bool { return !slices.Contains(r.Receipt.Snapshot.Loss.Reasons, reason) }) {
		return errors.New("qualification receipt does not establish the expected evidence coverage")
	}
	if r.TrialCase == "init-failure" {
		if r.WorkloadStarted || r.Resources[2].ID != "" {
			return errors.New("initialization fault did not precede workload creation")
		}
	} else if !r.WorkloadStarted || r.ReadySequence == 0 {
		return errors.New("qualification case never reached its intended workload")
	}
	if completeness == "complete" {
		return validateFunctionalQualification(r)
	}
	return nil
}

func (s *Store) Qualification(id string) (Qualification, error) {
	if !lowerHex(id, 64) {
		return Qualification{}, errors.New("invalid network qualification reference")
	}
	if err := s.intactAuthority(); err != nil {
		return Qualification{}, err
	}
	data, err := s.read("qualification-"+id+".json", maxQualificationBytes)
	if err != nil {
		return Qualification{}, err
	}
	var q Qualification
	if err := strictJSON(data, &q); err != nil {
		return Qualification{}, err
	}
	if q.Version == 1 && q.Contract == "visible-sni-tls443-v1" {
		canonical, err := canonicalQualificationFrame(q, false, true)
		encoded, _ := json.Marshal(q)
		if err != nil || q.ID != id || !bytes.Equal(encoded, data) || !equalJSON(canonical, q) || !hmac.Equal([]byte(s.qualificationID(q)), []byte(id)) {
			return Qualification{}, errors.New("invalid obsolete network qualification")
		}
		if err := s.confirmPublication(); err != nil {
			return Qualification{}, err
		}
		if err := s.intactAuthority(); err != nil {
			return Qualification{}, err
		}
		return Qualification{}, errObsoleteQualification
	}
	canonical, err := canonicalQualification(q)
	if err != nil || q.ID != id || !equalJSON(canonical, q) || !hmac.Equal([]byte(s.qualificationID(q)), []byte(id)) {
		return Qualification{}, errors.New("invalid owner-bound network qualification")
	}
	if err := s.confirmPublication(); err != nil {
		return Qualification{}, err
	}
	if err := s.intactAuthority(); err != nil {
		return Qualification{}, err
	}
	return q, nil
}

// RequirePolicy checks frozen provider/feature coverage without treating extra
// explicitly approved TLS destinations as new provider compatibility claims.
func (q Qualification) RequirePolicy(policy egress.Snapshot) error {
	return q.requirePolicy(policy, policy.Dependencies, nil)
}

func (q Qualification) requirePolicy(policy egress.Snapshot, selected []egress.Dependency, projection *string) error {
	if err := policy.RequireTLS443(true); err != nil {
		return err
	}
	for _, dependency := range selected {
		if !slices.Contains(policy.Dependencies, dependency) {
			return errors.New("selected dependency is absent from the captured network policy")
		}
		var features []string
		for _, grant := range policy.Grants {
			for _, origin := range grant.Origins {
				if origin.Provider == dependency.Provider && origin.Client == dependency.Client && origin.Backend == dependency.Backend &&
					origin.AuthMode == dependency.AuthMode && origin.BundleVersion == dependency.Version && origin.Feature != "" {
					features = append(features, origin.Feature)
				}
			}
		}
		covered := slices.ContainsFunc(q.Coverage, func(client QualifiedClient) bool {
			return client.Dependency == dependency && (projection == nil || client.MCPProjection == *projection) &&
				!slices.ContainsFunc(features, func(feature string) bool { return !slices.Contains(client.Features, feature) })
		})
		if !covered {
			return errors.New("selected provider, feature or MCP configuration has no matching completed network qualification")
		}
	}
	return nil
}

// RequireLaunch requires one tested combination of provider, features and MCP
// configuration. Separate successful combinations cannot be spliced together.
func (q Qualification) RequireLaunch(policy egress.Snapshot, projection string) error {
	return q.RequireSelection(policy, policy.Dependencies, projection)
}

// RequireSelection qualifies one exact catalog member against its union policy.
// Each selected dependency needs a single witness covering all applicable union
// features and this member's MCP projection, not a splice of separate trials.
func (q Qualification) RequireSelection(policy egress.Snapshot, selected []egress.Dependency, projection string) error {
	if projection != "none" && !lowerHex(projection, 64) {
		return errors.New("invalid captured MCP qualification projection")
	}
	if len(selected) == 0 && projection != "none" {
		return errors.New("MCP qualification requires a selected provider client")
	}
	return q.requirePolicy(policy, selected, &projection)
}

func dependencyReference(d egress.Dependency) string {
	return d.Provider + "@" + d.Version + "/" + string(d.Client) + "/" + d.Backend + "/" + d.AuthMode
}

func requireTrialClientPolicy(client *QualifiedClient, policy egress.Snapshot) error {
	if client == nil {
		return nil
	}
	if !slices.Contains(policy.Dependencies, client.Dependency) {
		return errors.New("qualification client is absent from its frozen trial policy")
	}
	for _, feature := range client.Features {
		found := false
		for _, grant := range policy.Grants {
			for _, origin := range grant.Origins {
				d := client.Dependency
				found = found || origin.Provider == d.Provider && origin.Client == d.Client && origin.Backend == d.Backend &&
					origin.AuthMode == d.AuthMode && origin.BundleVersion == d.Version && origin.Feature == feature
			}
		}
		if !found {
			return errors.New("qualification feature is absent from its frozen trial policy")
		}
	}
	return nil
}

func (t *QualificationTrial) caseFile(name string, client *QualifiedClient) string {
	digest := sha256.Sum256([]byte(name + "\x00" + qualificationKey(client)))
	return "qualification-case-" + t.group + "-" + hex.EncodeToString(digest[:]) + ".json"
}

// Reserve before publishing execution intent. Even an ambiguous failed attempt
// consumes this case: retry uses a new group, never cherry-picks a lucky pass.
func (t *QualificationTrial) reserveCase(name string, client *QualifiedClient, runID string) error {
	return t.store.publish(t.caseFile(name, client), []byte(runID), false)
}

func (t *QualificationTrial) RecordEvidence(runID string, data []byte) (string, error) {
	if err := t.intact(); err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > MaxQualificationEvidenceBytes || !json.Valid(data) {
		return "", errors.New("qualification observations require bounded JSON")
	}
	r, err := t.store.Execution(runID)
	if err != nil || r.Purpose != "qualification" || r.TrialGroup != t.group || r.CandidateID != t.candidate.ID || r.QualificationContract != QualificationContract || r.Receipt == nil {
		return "", errors.New("qualification observations require a sealed owned trial")
	}
	if err := validateQualificationEvidence(data, r.Receipt); err != nil {
		return "", err
	}
	name := "qualification-evidence-" + runID + ".json"
	if err := t.store.publish(name, data, false); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		prior, err := t.store.read(name, MaxQualificationEvidenceBytes)
		if err != nil || !bytes.Equal(prior, data) {
			return "", errors.New("qualification observations cannot be replaced")
		}
	}
	if err := t.store.confirmPublication(); err != nil {
		return "", err
	}
	if err := t.intact(); err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (t *QualificationTrial) CreateExecution(ctx context.Context, spec ExecutionSpec, caseName string, client *QualifiedClient) (Execution, error) {
	if err := t.intact(); err != nil {
		return Execution{}, err
	}
	if !validQualificationCase(caseName) || spec.QualificationID != "" || (client != nil) != slices.Contains([]string{"provider-start", "provider-resume", "mcp"}, caseName) {
		return Execution{}, errors.New("invalid private qualification trial case")
	}
	if client != nil {
		canonical, err := canonicalQualifiedClient(*client)
		if err != nil {
			return Execution{}, err
		}
		client = &canonical
		if caseName == "mcp" && client.MCPProjection == "none" {
			return Execution{}, errors.New("MCP qualification requires an enabled MCP projection")
		}
	}
	return t.store.createExecution(ctx, spec, t, caseName, client)
}
