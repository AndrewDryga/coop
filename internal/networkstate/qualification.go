package networkstate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

// QualificationContract changes when the host preflight changes what it accepts.
// Endpoint bundle releases and installed client closures have their own
// independent identities.
const QualificationContract = "visible-sni-tls-ports-v3"

const maxQualificationBytes = 256 << 10

// QualifiedClient is one client build the qualified image actually contains,
// read off the locked closure: the provider it belongs to, the client kind it
// launches and the exact package version. It is coverage, not a grant, and the
// version is a record of what was installed, not a per-host compatibility claim.
type QualifiedClient struct {
	Provider string        `json:"provider"`
	Client   egress.Client `json:"client"`
	Version  string        `json:"version"`
}

// QualificationProof is the ONE smoke run this host record rests on. The
// release's tagged runtime suite is the published fault and provider matrix;
// this proves the images work on this daemon. EvidenceDigest covers a retained
// bounded host observation, never agent output or a requester's passed:true.
type QualificationProof struct {
	RunID          string `json:"run_id"`
	Epoch          string `json:"epoch"`
	ReceiptDigest  string `json:"receipt_digest"`
	EvidenceDigest string `json:"evidence_digest"`
}

// Qualification is the per-host preflight record `coop net setup` publishes. It
// inlines the exact image pair and runtime binding it proves: an unproven build
// is not a reference anything can launch from.
type Qualification struct {
	Version     int                `json:"version"`
	ID          string             `json:"id"`
	Contract    string             `json:"contract"`
	Candidate   CandidateSpec      `json:"candidate"`
	Clients     []QualifiedClient  `json:"clients"`
	Smoke       QualificationProof `json:"smoke"`
	CompletedAt time.Time          `json:"completed_at"`
}

// QualificationSmoke is a host-only capability. Its zero value and serialized
// form confer no authority, and it cannot be reconstructed from a worker
// request. A crashed setup is not resumable: cleanup runs through execution
// custody and the whole preflight is simply rerun.
type QualificationSmoke struct {
	store     *Store
	candidate CandidateSpec
	clients   []QualifiedClient
	runID     string
}

func (t *QualificationSmoke) Candidate() CandidateSpec {
	if t == nil {
		return CandidateSpec{}
	}
	return t.candidate
}

// BeginQualification opens the preflight for one constructed image pair and the
// client builds its closure installed. Nothing is published until the smoke run
// proves the pair on this daemon.
func (s *Store) BeginQualification(candidate CandidateSpec, clients []QualifiedClient) (*QualificationSmoke, error) {
	if err := s.intactAuthority(); err != nil {
		return nil, err
	}
	candidate, err := canonicalCandidate(candidate)
	if err != nil {
		return nil, err
	}
	clients, err = canonicalQualifiedClients(clients)
	if err != nil {
		return nil, err
	}
	return &QualificationSmoke{store: s, candidate: candidate, clients: clients}, nil
}

func (t *QualificationSmoke) intact() error {
	if t == nil || t.store == nil {
		return errors.New("qualification requires a private host preflight")
	}
	candidate, err := canonicalCandidate(t.candidate)
	if err != nil || !equalJSON(candidate, t.candidate) {
		return errors.New("qualification candidate is invalid or changed")
	}
	return t.store.intactAuthority()
}

// CreateExecution registers the smoke through the ordinary engine, so what the
// preflight proves is exactly what a workload later gets. One preflight runs one
// smoke: a second attempt needs a new setup.
func (t *QualificationSmoke) CreateExecution(ctx context.Context, spec ExecutionSpec) (Execution, error) {
	if err := t.intact(); err != nil {
		return Execution{}, err
	}
	if spec.QualificationID != "" || t.runID != "" {
		return Execution{}, errors.New("invalid private network preflight launch")
	}
	record, err := t.store.createExecution(ctx, spec, t)
	if record.ID != "" {
		t.runID = record.ID
	}
	return record, err
}

func (t *QualificationSmoke) RecordEvidence(runID string, data []byte) (string, error) {
	if err := t.intact(); err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > MaxQualificationEvidenceBytes || !json.Valid(data) {
		return "", errors.New("qualification observations require bounded JSON")
	}
	r, err := t.store.Execution(runID)
	if err != nil || runID != t.runID || r.Purpose != "qualification" || r.ClientImage != t.candidate.ClientImage ||
		r.QualificationContract != QualificationContract || r.Receipt == nil {
		return "", errors.New("qualification observations require a sealed owned smoke run")
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

// Complete publishes the record once the smoke's own sealed receipt shows the
// workload ran to a clean end, nothing is still owned, the proxy accounted for
// every byte, and the gateway both allowed the smoke domain and denied
// something. Later reads never depend on receipts that may legitimately expire.
func (t *QualificationSmoke) Complete(domain string) (Qualification, error) {
	if err := t.intact(); err != nil {
		return Qualification{}, err
	}
	r, err := t.store.Execution(t.runID)
	if err != nil {
		return Qualification{}, err
	}
	if r.Purpose != "qualification" || r.QualificationContract != QualificationContract ||
		r.ClientImage != t.candidate.ClientImage || r.GatewayImage != t.candidate.GatewayImage ||
		r.DaemonID != t.candidate.Runtime.DaemonID || r.Endpoint != t.candidate.Runtime.Endpoint {
		return Qualification{}, errors.New("smoke run does not match the constructed candidate")
	}
	if err := validateSmokeOutcome(r, domain); err != nil {
		return Qualification{}, err
	}
	evidence, err := t.store.read("qualification-evidence-"+r.ID+".json", MaxQualificationEvidenceBytes)
	if err != nil {
		return Qualification{}, errors.New("qualification observation artifact is unavailable")
	}
	if err := validateQualificationEvidence(evidence, r.Receipt); err != nil {
		return Qualification{}, err
	}
	digest := sha256.Sum256(evidence)
	q := Qualification{Version: 1, Contract: QualificationContract, Candidate: t.candidate, Clients: t.clients,
		Smoke:       QualificationProof{RunID: r.ID, Epoch: r.Epoch, ReceiptDigest: r.Receipt.Digest, EvidenceDigest: hex.EncodeToString(digest[:])},
		CompletedAt: *r.Receipt.EndedAt}
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

// validateSmokeOutcome reads the sealed receipt only. A caller cannot assert any
// of it: "complete" already means terminal, unlossy, exactly covered evidence
// whose agent container is gone.
func validateSmokeOutcome(r Execution, domain string) error {
	if r.Receipt == nil || r.Receipt.EndedAt == nil || r.Receipt.Finality != "final" ||
		r.Receipt.Workload != "exited" || r.Receipt.Completeness != "complete" {
		return errors.New("network setup requires a final sealed receipt of a clean smoke run")
	}
	if !r.WorkloadStarted || r.ReadySequence == 0 {
		return errors.New("smoke run never reached its workload behind a ready gateway")
	}
	for _, resource := range r.Resources {
		if resource.State != "gone" {
			return errors.New("smoke run still owns unresolved runtime resources")
		}
	}
	if r.Artifact.State != "gone" {
		return errors.New("smoke run still owns unresolved launch artifacts")
	}
	s := r.Receipt.Snapshot
	if s.Coverage.ProxyBytes.Status != "exact" || s.Coverage.ProxyBytes.Reason != "" {
		return errors.New("smoke run did not account for every proxied byte")
	}
	allowed := slices.ContainsFunc(s.Connections, func(c networkview.Connection) bool {
		return c.Name == domain && c.NameSource == "sni" && c.State == "closed" && !c.Partial && c.RuleID != "" &&
			c.SentBytes != nil && *c.SentBytes > 0 && c.ReceivedBytes != nil && *c.ReceivedBytes > 0
	})
	denied := slices.ContainsFunc(s.Denials, func(d networkview.Denial) bool { return d.Basis == "observed" })
	if !allowed || !denied {
		return errors.New("smoke run lacks a measured allowed connection or an observed denial")
	}
	return nil
}

func canonicalQualifiedClient(client QualifiedClient) (QualifiedClient, error) {
	if !safeRecordToken(client.Provider, 64) || client.Client != egress.ClientCLI && client.Client != egress.ClientACP ||
		!safeRecordToken(client.Version, 128) {
		return QualifiedClient{}, errors.New("invalid qualified client identity")
	}
	return client, nil
}

func canonicalQualifiedClients(clients []QualifiedClient) ([]QualifiedClient, error) {
	if len(clients) == 0 || len(clients) > 32 {
		return nil, errors.New("the qualified image must contain between one and 32 client builds")
	}
	clients = slices.Clone(clients)
	for i, client := range clients {
		var err error
		if clients[i], err = canonicalQualifiedClient(client); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(clients, func(a, b QualifiedClient) int {
		data, _ := json.Marshal(a)
		other, _ := json.Marshal(b)
		return strings.Compare(string(data), string(other))
	})
	for i := 1; i < len(clients); i++ {
		if clients[i-1] == clients[i] {
			return nil, errors.New("duplicate qualified client build")
		}
	}
	return clients, nil
}

func canonicalQualification(q Qualification) (Qualification, error) {
	if q.Version != 1 || q.Contract != QualificationContract || q.CompletedAt.IsZero() ||
		!lowerHex(q.Smoke.RunID, 32) || !lowerHex(q.Smoke.Epoch, 32) ||
		!lowerHex(q.Smoke.ReceiptDigest, 64) || !lowerHex(q.Smoke.EvidenceDigest, 64) {
		return Qualification{}, errors.New("a setup record on this host is unreadable — run 'coop net setup'")
	}
	var err error
	if q.Candidate, err = canonicalCandidate(q.Candidate); err != nil {
		return Qualification{}, err
	}
	if q.Clients, err = canonicalQualifiedClients(q.Clients); err != nil {
		return Qualification{}, err
	}
	return q, nil
}

func (s *Store) qualificationID(q Qualification) string {
	q.ID = ""
	data, _ := json.Marshal(q)
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("network-qualification-v1\x00"))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Store) Qualification(id string) (Qualification, error) {
	if !lowerHex(id, 64) {
		return Qualification{}, errors.New("that is not a setup record id")
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
	canonical, err := canonicalQualification(q)
	if err != nil || q.ID != id || !equalJSON(canonical, q) || !hmac.Equal([]byte(s.qualificationID(q)), []byte(id)) {
		return Qualification{}, errors.New("a setup record on this host is unreadable, or was not written by this coop — run 'coop net setup'")
	}
	if err := s.confirmPublication(); err != nil {
		return Qualification{}, err
	}
	if err := s.intactAuthority(); err != nil {
		return Qualification{}, err
	}
	return q, nil
}

// Qualifications returns the host's completed records, newest first. It lists
// the private directory and performs no probe, canary or runtime mutation.
func (s *Store) Qualifications(ctx context.Context) ([]Qualification, error) {
	if err := s.intactAuthority(); err != nil {
		return nil, err
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(65536)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) == 65536 {
		return nil, errors.New("network qualification index exceeds its directory scan limit")
	}
	var result []Qualification
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		base, ok := strings.CutSuffix(name, ".json")
		if !ok {
			continue
		}
		id, ok := strings.CutPrefix(base, "qualification-")
		if !ok || !lowerHex(id, 64) {
			continue
		}
		// A record from an earlier enforcement contract is not this host's setup: its
		// canonical frame and owner-bound id no longer verify under the current rules.
		// Skip it instead of failing the whole listing, so one retired record cannot
		// take every net verb down; the current-contract check in RequireLaunch still
		// refuses to launch on it, and a launch then sets the host up again.
		if !s.qualificationIsCurrent(name) {
			continue
		}
		q, err := s.Qualification(id)
		if err != nil {
			return nil, err
		}
		if len(result) >= 256 {
			return nil, errors.New("network qualification index exceeds its retained record limit")
		}
		result = append(result, q)
	}
	slices.SortFunc(result, func(a, b Qualification) int { return b.CompletedAt.Compare(a.CompletedAt) })
	return result, nil
}

// RequireLaunch is the per-host preflight match. The release's bundle metadata
// is the version claim for a provider's endpoints; what this host proved is that
// the image it will launch contains that provider's selected client at all.
func (q Qualification) RequireLaunch(policy egress.Snapshot) error {
	if q.Contract != QualificationContract {
		return errors.New("this host's network setup was made by an older coop")
	}
	if err := policy.RequireSupported(); err != nil {
		return err
	}
	for _, dependency := range policy.Dependencies {
		if !slices.ContainsFunc(q.Clients, func(client QualifiedClient) bool {
			return client.Provider == dependency.Provider && client.Client == dependency.Client
		}) {
			return errors.New("the box image this host was set up with has no " + dependency.Provider + " " + string(dependency.Client) + " in it")
		}
	}
	return nil
}

func dependencyReference(d egress.Dependency) string {
	return d.Provider + "@" + d.Version + "/" + string(d.Client) + "/" + d.Backend + "/" + d.AuthMode
}

// qualificationIsCurrent reports whether the record file names the current contract.
// It reads only the contract field; verification happens in Qualification.
func (s *Store) qualificationIsCurrent(name string) bool {
	data, err := s.read(name, maxQualificationBytes)
	if err != nil {
		return false
	}
	var head struct {
		Contract string `json:"contract"`
	}
	return json.Unmarshal(data, &head) == nil && head.Contract == QualificationContract
}
