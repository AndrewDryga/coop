package networkstate

import (
	"bytes"
	"context"
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

// qualificationVersion is the setup record format this coop writes and reads. A record of another
// version is another coop's: `coop net setup` writes a current one.
const qualificationVersion = 2

// QualifiedClient is one client build the qualified image actually contains,
// read off the locked closure: the provider it belongs to, the client kind it
// launches and the exact package version. It is coverage, not a grant, and the
// version is a record of what was installed, not a per-host compatibility claim.
type QualifiedClient struct {
	Provider string        `json:"provider"`
	Client   egress.Client `json:"client"`
	Version  string        `json:"version"`
}

// Qualification is the per-host record `coop net setup` writes once its smoke run passed: the
// exact image pair and runtime binding it proved, the client builds that image holds, and the run
// that proved them (SmokeRun). The release's tagged runtime suite is the published fault and
// provider matrix; this proves the images work on this daemon. Its id is the hash of the rest:
// what makes it authority is that only coop writes the store, which no box can reach.
type Qualification struct {
	Version     int               `json:"version"`
	ID          string            `json:"id"`
	Contract    string            `json:"contract"`
	Candidate   CandidateSpec     `json:"candidate"`
	Clients     []QualifiedClient `json:"clients"`
	SmokeRun    string            `json:"smoke_run"`
	CompletedAt time.Time         `json:"completed_at"`
}

// QualificationSmoke is one setup in progress: the image pair and client builds it is proving.
// Only the setup workflow makes one, and it is nothing until Complete has read the smoke run's own
// sealed receipt. A crashed setup is not resumable: cleanup runs through execution custody and
// the whole preflight is simply rerun.
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

func (t *QualificationSmoke) open() error {
	if t == nil || t.store == nil {
		return errors.New("qualification requires a private host preflight")
	}
	return nil
}

// CreateExecution registers the smoke through the ordinary engine, so what the
// preflight proves is exactly what a workload later gets. One preflight runs one
// smoke: a second attempt needs a new setup.
func (t *QualificationSmoke) CreateExecution(ctx context.Context, spec ExecutionSpec) (Execution, error) {
	if err := t.open(); err != nil {
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

// Complete writes the record once the smoke's own sealed receipt shows the
// workload ran to a clean end, nothing is still owned, the proxy accounted for
// every byte, and the gateway both allowed the smoke domain and denied
// something. Later reads never depend on receipts that may legitimately expire.
func (t *QualificationSmoke) Complete(domain string) (Qualification, error) {
	if err := t.open(); err != nil {
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
	q := Qualification{Version: qualificationVersion, Contract: QualificationContract, Candidate: t.candidate,
		Clients: t.clients, SmokeRun: r.ID, CompletedAt: *r.Receipt.EndedAt}
	q, err = canonicalQualification(q)
	if err != nil {
		return Qualification{}, err
	}
	q.ID = qualificationID(q)
	data, err := json.Marshal(q)
	if err != nil || len(data) > maxQualificationBytes {
		return Qualification{}, errors.New("network qualification exceeds its byte bound")
	}
	name := "qualification-" + q.ID + ".json"
	if err := t.store.publish(name, data, false); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return Qualification{}, err
		}
		prior, err := t.store.read(name)
		if err != nil || !bytes.Equal(prior, data) {
			return Qualification{}, errors.New("network qualification identity collision or invalid content")
		}
	}
	return q, t.store.checkRootPath()
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
	if q.Version != qualificationVersion || q.Contract != QualificationContract || q.CompletedAt.IsZero() ||
		!lowerHex(q.SmokeRun, 32) {
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

func qualificationID(q Qualification) string {
	q.ID = ""
	data, _ := json.Marshal(q)
	sum := sha256.Sum256(append([]byte("network-qualification-v2\x00"), data...))
	return hex.EncodeToString(sum[:])
}

func (s *Store) Qualification(id string) (Qualification, error) {
	if !lowerHex(id, 64) {
		return Qualification{}, errors.New("that is not a setup record id")
	}
	if err := s.intactAuthority(); err != nil {
		return Qualification{}, err
	}
	data, err := s.read("qualification-" + id + ".json")
	if err != nil {
		return Qualification{}, err
	}
	var q Qualification
	if err := json.Unmarshal(data, &q); err != nil {
		return Qualification{}, err
	}
	if q.Version != qualificationVersion {
		return Qualification{}, errors.New("this host's network setup was made by an older coop — run 'coop net setup'")
	}
	canonical, err := canonicalQualification(q)
	if err != nil || q.ID != id || !equalJSON(canonical, q) || qualificationID(q) != id {
		return Qualification{}, errors.New("a setup record on this host is unreadable — run 'coop net setup'")
	}
	return q, s.checkRootPath()
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
		// A record from an earlier enforcement contract or record version is not this host's
		// setup: its canonical frame and id no longer verify under the current rules.
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

// qualificationIsCurrent reports whether the record file is this coop's: the current format and
// contract. It reads only those fields; verification happens in Qualification.
func (s *Store) qualificationIsCurrent(name string) bool {
	data, err := s.read(name)
	if err != nil {
		return false
	}
	var head struct {
		Version  int    `json:"version"`
		Contract string `json:"contract"`
	}
	return json.Unmarshal(data, &head) == nil && head.Version == qualificationVersion && head.Contract == QualificationContract
}
