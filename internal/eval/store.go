package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Results live in an owner-private directory OUTSIDE every candidate mount, so a candidate can never
// read or write the record of its own trial. The layout is plain files — no database: one directory
// per run, a manifest written before any work, one record per trial (so concurrent workers never
// contend on a shared file), and an atomic summary sealed only when the whole run is done. A trial
// record is written at START (so an interrupted run is visibly incomplete, not silently missing) and
// finalized in place when the trial finishes.
//
// The store owns durability and layout; grading, comparison and the trial lifecycle are elsewhere.
// Schema versions are explicit so a later, richer record format is a new version, never a silent
// reinterpretation of an old one.

const (
	// runSchema and trialSchema version the on-disk records independently of the Coop build.
	runSchema   = 1
	trialSchema = 1

	manifestName = "run.json"
	summaryName  = "summary.json"
	trialsDir    = "trials"
)

// TrialStatus is where a trial ended. Every status is explicit — a run's denominator is the full
// requested matrix, so a trial that never started or errored in the harness is counted, not dropped.
type TrialStatus string

const (
	TrialPending  TrialStatus = "pending" // recorded at run start, before the worker reached it
	TrialRunning  TrialStatus = "running" // a worker started it (written before launch)
	TrialPassed   TrialStatus = "passed"  // executed and graded, all required outcomes met
	TrialFailed   TrialStatus = "failed"  // executed and graded, a required outcome unmet
	TrialTimedOut TrialStatus = "timed_out"
	TrialError    TrialStatus = "error" // a harness/grading error, not a model-quality result
)

// RunRecord is the manifest written before any trial launches: the fixed requested matrix and the
// identity of everything a comparison must be able to tell apart. It is never rewritten after start
// (the summary seals the outcome); the per-trial records carry what happened.
type RunRecord struct {
	Schema    int         `json:"schema"`
	ID        string      `json:"id"`
	CreatedAt time.Time   `json:"created_at"`
	Suite     string      `json:"suite"`
	Runner    Runner      `json:"runner"`
	Workload  string      `json:"workload_fingerprint"`
	Repeat    int         `json:"repeat"`
	Jobs      int         `json:"jobs"`
	TimeoutMS int64       `json:"timeout_ms"`
	Cases     []string    `json:"cases"`
	Configs   []RunConfig `json:"configs"`
}

// RunConfig is one evaluated configuration as recorded on the run: its label and the fingerprints a
// before/after comparison keys on.
type RunConfig struct {
	Kind        ConfigKind `json:"kind"`
	Label       string     `json:"label"`
	Fingerprint string     `json:"config_fingerprint"`
	Build       string     `json:"build"`
}

// TrialRecord is one (case x configuration x repetition). Written as pending/running before launch,
// finalized in place after. Durations and score are filled when known; a missing measurement is left
// zero-valued with a status that says why, never faked.
type TrialRecord struct {
	Schema      int         `json:"schema"`
	RunID       string      `json:"run_id"`
	Case        string      `json:"case"`
	ConfigIndex int         `json:"config_index"`
	ConfigLabel string      `json:"config_label"`
	Repetition  int         `json:"repetition"`
	Status      TrialStatus `json:"status"`
	StartedAt   time.Time   `json:"started_at,omitzero"`
	EndedAt     time.Time   `json:"ended_at,omitzero"`
	Order       int         `json:"order"`
	Detail      string      `json:"detail,omitempty"`
}

// TrialKey identifies a trial within a run, and names its record file.
func TrialKey(caseID string, configIndex, repetition int) string {
	return fmt.Sprintf("%s__c%d__r%d", caseID, configIndex, repetition)
}

// Store is one run's private directory.
type Store struct {
	dir string
	id  string
}

// CreateRun makes the run directory under root and writes its manifest. root is the owner-private
// eval state directory (the CLI resolves it under XDG state; a test passes a temp dir). The run id
// is time-ordered so `ls` sorts runs chronologically, and carries the workload fingerprint's prefix
// so two runs of the same suite are recognizable.
func CreateRun(root string, rec RunRecord) (*Store, error) {
	if rec.ID == "" {
		return nil, errors.New("run record has no id")
	}
	if filepath.Base(rec.ID) != rec.ID {
		return nil, fmt.Errorf("invalid run id %q", rec.ID)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, rec.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("eval run %s already exists", rec.ID)
		}
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(dir, trialsDir), 0o700); err != nil {
		return nil, err
	}
	rec.Schema = runSchema
	if err := writeJSONAtomic(filepath.Join(dir, manifestName), rec); err != nil {
		return nil, err
	}
	return &Store{dir: dir, id: rec.ID}, nil
}

// ID is the run's identifier.
func (s *Store) ID() string { return s.id }

// Dir is the run's private directory.
func (s *Store) Dir() string { return s.dir }

// WriteTrial writes (or overwrites) one trial's record. A trial is written pending at run start,
// running before launch, and finalized after — each an atomic replace of its own file, so a
// concurrent worker on another trial never contends and a crash leaves the last durable state.
func (s *Store) WriteTrial(rec TrialRecord) error {
	rec.Schema = trialSchema
	rec.RunID = s.id
	name := TrialKey(rec.Case, rec.ConfigIndex, rec.Repetition) + ".json"
	return writeJSONAtomic(filepath.Join(s.dir, trialsDir, name), rec)
}

// Seal writes the run's final summary atomically, once every trial is finalized. Its presence is
// what marks a run complete; a run directory without it was interrupted.
func (s *Store) Seal(summary RunSummary) error {
	summary.Schema = runSchema
	summary.RunID = s.id
	summary.SealedAt = time.Now().UTC()
	return writeJSONAtomic(filepath.Join(s.dir, summaryName), summary)
}

// RunSummary is the sealed outcome: per-status counts over the full requested matrix, so a reader
// sees coverage (how many of the requested trials actually produced a graded result) beside any
// pass counts. Never improve a number by dropping a requested trial from the denominator.
type RunSummary struct {
	Schema    int                 `json:"schema"`
	RunID     string              `json:"run_id"`
	SealedAt  time.Time           `json:"sealed_at"`
	Requested int                 `json:"requested"`
	Counts    map[TrialStatus]int `json:"counts"`
}

// LoadRun reads a run's manifest.
func LoadRun(root, id string) (*RunRecord, error) {
	if filepath.Base(id) != id {
		return nil, fmt.Errorf("invalid run id %q", id)
	}
	var rec RunRecord
	if err := readJSON(filepath.Join(root, id, manifestName), &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// LoadTrials reads every trial record of a run, in stable key order.
func LoadTrials(root, id string) ([]TrialRecord, error) {
	if filepath.Base(id) != id {
		return nil, fmt.Errorf("invalid run id %q", id)
	}
	dir := filepath.Join(root, id, trialsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []TrialRecord
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var rec TrialRecord
		if err := readJSON(filepath.Join(dir, e.Name()), &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Case != b.Case {
			return a.Case < b.Case
		}
		if a.ConfigIndex != b.ConfigIndex {
			return a.ConfigIndex < b.ConfigIndex
		}
		return a.Repetition < b.Repetition
	})
	return out, nil
}

// LoadSummary reads a run's sealed summary; ok=false if the run is not sealed (interrupted).
func LoadSummary(root, id string) (*RunSummary, bool, error) {
	if filepath.Base(id) != id {
		return nil, false, fmt.Errorf("invalid run id %q", id)
	}
	var sum RunSummary
	err := readJSON(filepath.Join(root, id, summaryName), &sum)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &sum, true, nil
}

// ListRuns returns the run ids under root, newest first (the ids are time-ordered).
func ListRuns(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// writeJSONAtomic writes v as indented JSON via a temp file + rename, so a reader never sees a
// half-written record and a crash leaves the previous version intact.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// NewRunID makes a time-ordered run id that also carries the workload fingerprint's prefix, so `ls`
// sorts runs chronologically and two runs of the same suite are recognizable at a glance. The
// timestamp is UTC to a second plus a short workload tag; a second run in the same second gets a
// distinct id from the caller's uniqueness (the store refuses a collision).
func NewRunID(now time.Time, workload Fingerprint) string {
	tag := string(workload)
	if len(tag) > 8 {
		tag = tag[:8]
	}
	if tag == "" {
		tag = "nofp"
	}
	return now.UTC().Format("20060102T150405Z") + "-" + tag
}

// NewRunRecord builds the record that identifies a run from its plan and frozen configurations, so
// a caller never hand-assembles the fingerprints a later comparison keys on. The workload
// fingerprint comes from the PARSED suite, and each configuration carries its own fingerprint and
// the Coop build it ran under.
func NewRunRecord(plan *Plan, frozen []FrozenConfig, now time.Time) RunRecord {
	rec := RunRecord{
		Schema:    runSchema,
		CreatedAt: now.UTC(),
		Suite:     plan.Suite.Name,
		Runner:    plan.Suite.Runner,
		Workload:  string(WorkloadFingerprint(plan.Suite)),
		Repeat:    plan.Repeat,
		Jobs:      plan.Jobs,
		TimeoutMS: plan.Timeout.Milliseconds(),
	}
	for _, c := range plan.Suite.Cases {
		rec.Cases = append(rec.Cases, c.ID)
	}
	for _, f := range frozen {
		rec.Configs = append(rec.Configs, RunConfig{
			Kind: f.Kind, Label: f.Label,
			Fingerprint: string(f.Fingerprint()), Build: f.Build.Version,
		})
	}
	rec.ID = NewRunID(now, Fingerprint(rec.Workload))
	return rec
}
