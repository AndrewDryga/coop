package forkspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/fsidentity"
	"github.com/AndrewDryga/coop/internal/randid"
)

const (
	forkGenerationVersion         = 3
	forkGenerationIsolatedVersion = 4
	forkGenerationLimit           = 4096
	forkGenerationCount           = 4096
	// GenerationMarkerName is the ignored workspace-side half of a fork's private hardlink
	// identity. Destructive workspace maintenance must preserve it; validation still requires
	// the matching private anchor and generation record.
	GenerationMarkerName = ".coop-fork-generation"
)

var forkGenerationRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Generation is an immutable incarnation of one named fork. A name can be reused after --fresh
// or rm; a generation cannot, so stale workers, boxes, task assignments, and candidates cannot
// silently attach themselves to the replacement workspace.
type Generation string

type Identity struct {
	Name       string     `json:"name"`
	Generation Generation `json:"generation"`
}

// generationRecord binds a fork's logical generation to one physical workspace. Version 3 names a
// private hardlink anchor whose live inode is compared with the marker in the workspace on every
// authoritative open. Versions 1 and 2, the inode and inode+birth-time formats, are refused by name
// (refuseOldGenerationRecord).
type generationRecord struct {
	Version         int        `json:"version"`
	Name            string     `json:"name"`
	Generation      Generation `json:"generation"`
	WorkspaceAnchor string     `json:"workspace_anchor,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	Isolated        bool       `json:"isolated,omitempty"`
	CreationBase    string     `json:"creation_base,omitempty"`
}

func GenerationPath(repo, name string) string {
	return filepath.Join(StateDir(repo), name+".generation.json")
}

// LandIntentPath is shared only as a destructive-lifecycle guard. forkctl owns and validates the
// journal contents; lower-level session teardown merely treats its exact-generation presence as a
// reason to refuse deletion and require merge recovery.
func LandIntentPath(repo string, identity Identity) string {
	return filepath.Join(StateDir(repo), identity.Name+"."+string(identity.Generation)+".land.json")
}

func NewGeneration() Generation { return Generation(randid.Hex(16)) }

func ValidGeneration(g Generation) bool { return forkGenerationRE.MatchString(string(g)) }

func forkGenerationAnchorName(_ string, generation Generation) string {
	// The random generation is already unique within this repository. Keeping the human fork name
	// out of the private filename means any name that fits its .generation.json record at NAME_MAX
	// fits here too; the authenticated marker body still binds both name and generation.
	return "generation-" + string(generation) + ".anchor"
}

func forkGenerationAnchorBody(name string, generation Generation) []byte {
	return []byte("coop-fork-generation-v3\n" + name + "\n" + string(generation) + "\n")
}

func generationAnchorName(record generationRecord) string {
	if record.Isolated {
		// Marker bytes are writable through the public hardlink. The private filename binds
		// mode/base too, so missing-record recovery cannot authorize a rewritten contract.
		return "generation-" + string(record.Generation) + "-isolated-" + record.CreationBase + ".anchor"
	}
	return forkGenerationAnchorName(record.Name, record.Generation)
}

// sameDisk reports whether two existing folders are on one filesystem; tests replace it.
var sameDisk = func(a, b string) (bool, error) {
	first, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	second, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	x, xok := first.Sys().(*syscall.Stat_t)
	y, yok := second.Sys().(*syscall.Stat_t)
	return xok && yok && x.Dev == y.Dev, nil
}

// requireOneDisk refuses a fork whose folder is on another disk than Coop's private state folder:
// a fork is bound to its record by a hard link from its folder, and a hard link cannot cross disks,
// the rule network and service approvals already have.
func requireOneDisk(repo, name string) error {
	one, err := sameDisk(Workspace(repo, name), StateDir(repo))
	if err != nil {
		return err
	}
	if !one {
		return fmt.Errorf("forks need the fork's folder %s and Coop's fork records folder %s on one disk, and these are on different disks; keep the project on the disk that holds your home folder to use forks", Workspace(repo, name), StateDir(repo))
	}
	return nil
}

func forkGenerationBinding(repo string, record generationRecord, state *os.Root) fsidentity.Binding {
	body := forkGenerationAnchorBody(record.Name, record.Generation)
	if record.Isolated {
		body = []byte("coop-fork-generation-v4-isolated\n" + record.Name + "\n" + string(record.Generation) + "\n" + record.CreationBase + "\n")
	}
	return fsidentity.Binding{
		RootPath: Workspace(repo, record.Name), MarkerName: GenerationMarkerName,
		AnchorRoot: state, AnchorName: generationAnchorName(record),
		Body: body,
	}
}

func validateGenerationRecord(record generationRecord, name string) error {
	if record.Name != name || !ValidExistingName(name) || !ValidGeneration(record.Generation) ||
		record.CreatedAt.IsZero() {
		return errors.New("invalid fork generation record")
	}
	if record.Isolated != (record.Version == forkGenerationIsolatedVersion) {
		return errors.New("fork isolation does not match its authority version")
	}
	if record.Isolated && !ValidObjectID(record.CreationBase) || !record.Isolated && record.CreationBase != "" {
		return errors.New("fork creation base does not match its isolation authority")
	}
	switch record.Version {
	case forkGenerationVersion, forkGenerationIsolatedVersion:
		if record.WorkspaceAnchor != generationAnchorName(record) {
			return errors.New("invalid anchored fork generation record")
		}
	default:
		return errors.New("unsupported fork generation record")
	}
	return nil
}

// refuseOldGenerationRecord names a version 1 or 2 record, which only a build up to v12.1.0
// converts, and leaves the file as it is. The refusal is an error, never an absent record, so the
// fork is not read as unbound or free, and fork stop reports it instead of acting on it.
func refuseOldGenerationRecord(data []byte, name string) error {
	var header struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(data, &header) != nil || header.Version != 1 && header.Version != 2 {
		return nil
	}
	return fmt.Errorf("fork %s has a version %d generation record from Coop v10.1.2 or earlier, which this Coop no longer converts — Coop left it unchanged; stop the fork (coop fork stop %s), then start it once with Coop v12.1.0, which anchors it as version 3, then retry", name, header.Version, name)
}

// Stays bounded and no-follow, as every read of a record a box could once write.
func readGenerationRecord(repo, name string) (generationRecord, error) {
	path := GenerationPath(repo, name)
	data, err := ReadRecord(path, fmt.Sprintf("fork generation record %q", path), forkGenerationLimit)
	if errors.Is(err, os.ErrNotExist) {
		// No record here proves none only once the old folder holds none: a build before the move
		// kept them there.
		if oldErr := RequireNoOldForkRecords(repo); oldErr != nil {
			return generationRecord{}, oldErr
		}
	}
	if err != nil {
		return generationRecord{}, err
	}
	if err := refuseOldGenerationRecord(data, name); err != nil {
		return generationRecord{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var record generationRecord
	if err := dec.Decode(&record); err != nil {
		return generationRecord{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return generationRecord{}, errors.New("fork generation record contains multiple JSON values")
		}
		return generationRecord{}, err
	}
	if err := validateGenerationRecord(record, name); err != nil {
		return generationRecord{}, err
	}
	return record, nil
}

// ReadGeneration reads host-owned identity without requiring the workspace to exist. That matters
// during stop/recovery: a crashed worker's exact runtime labels remain recoverable even when its
// workspace was removed out of band. ValidateGenerationWorkspace is the mutation-time check.
func ReadGeneration(repo, name string) (Identity, bool, error) {
	record, err := readGenerationRecord(repo, name)
	if errors.Is(err, os.ErrNotExist) {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, err
	}
	return Identity{Name: record.Name, Generation: record.Generation}, true, nil
}

// IsolatedGeneration reads the host-owned execution boundary, never workspace configuration.
func IsolatedGeneration(repo string, identity Identity) (bool, error) {
	record, err := readGenerationRecord(repo, identity.Name)
	if err != nil {
		return false, err
	}
	if record.Generation != identity.Generation {
		return false, errors.New("fork generation changed")
	}
	return record.Isolated, nil
}

// IsolatedCreationBase is the immutable source boundary before the first publication.
func IsolatedCreationBase(repo string, identity Identity) (string, error) {
	record, err := readGenerationRecord(repo, identity.Name)
	if err != nil {
		return "", err
	}
	if record.Generation != identity.Generation || !record.Isolated {
		return "", errors.New("fork has no matching isolated creation base")
	}
	return record.CreationBase, nil
}

// GenerationCreatedAt is when coop bound this exact incarnation, read from the same immutable
// record that proves ownership. Reclamation ages a candidate off this durable stamp and never off
// a directory's modification time, which any process inside the workspace can move.
func GenerationCreatedAt(repo string, identity Identity) (time.Time, error) {
	record, err := readGenerationRecord(repo, identity.Name)
	if err != nil {
		return time.Time{}, err
	}
	if record.Generation != identity.Generation {
		return time.Time{}, errors.New("fork generation changed")
	}
	return record.CreatedAt, nil
}

// GenerationIdentities enumerates every host-owned incarnation, including a generation whose
// workspace disappeared out of band. Per-record failures remain visible without hiding healthy
// siblings; callers must not turn a malformed record into permission to mutate anything.
func GenerationIdentities(repo string) ([]Identity, []error) {
	root, err := OpenStateRoot(repo, "", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	defer root.Close()
	dir, err := root.OpenFile(".", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, []error{err}
	}
	entries, readErr := dir.ReadDir(forkGenerationCount + 1)
	closeErr := dir.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, []error{errors.Join(readErr, closeErr)}
	}
	if closeErr != nil {
		return nil, []error{closeErr}
	}
	if len(entries) > forkGenerationCount {
		return nil, []error{fmt.Errorf("fork control directory exceeds %d entries", forkGenerationCount)}
	}
	var identities []Identity
	var problems []error
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".generation.json")
		if !ok {
			continue
		}
		if entry.IsDir() || !ValidExistingName(name) {
			problems = append(problems, fmt.Errorf("%s: invalid fork generation entry", entry.Name()))
			continue
		}
		identity, present, err := ReadGeneration(repo, name)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", entry.Name(), err))
			continue
		}
		if present {
			identities = append(identities, identity)
		}
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Name != identities[j].Name {
			return identities[i].Name < identities[j].Name
		}
		return identities[i].Generation < identities[j].Generation
	})
	return identities, problems
}

func ValidateGenerationWorkspace(repo string, identity Identity) error {
	record, err := readGenerationRecord(repo, identity.Name)
	if err != nil {
		return err
	}
	if record.Generation != identity.Generation {
		return errors.New("fork generation changed")
	}
	state, err := OpenStateRoot(repo, "", false)
	if err != nil {
		return err
	}
	defer state.Close()
	root, err := fsidentity.Open(forkGenerationBinding(repo, record, state))
	if err != nil {
		return fmt.Errorf("fork workspace no longer matches its generation: %w", err)
	}
	return root.Close()
}

// OpenGenerationWorkspaceRoot opens the exact anchored workspace bound to identity. Callers that
// cross a sandbox trust boundary must perform their relative filesystem operations through this
// handle: validating a pathname and then reopening it by name would let an agent swap a parent
// directory or symlink between those two steps.
func OpenGenerationWorkspaceRoot(repo string, identity Identity) (*os.Root, error) {
	record, err := readGenerationRecord(repo, identity.Name)
	if err != nil {
		return nil, err
	}
	if record.Generation != identity.Generation {
		return nil, errors.New("fork generation changed")
	}
	state, err := OpenStateRoot(repo, "", false)
	if err != nil {
		return nil, err
	}
	root, openErr := fsidentity.Open(forkGenerationBinding(repo, record, state))
	closeErr := state.Close()
	if openErr != nil || closeErr != nil {
		if root != nil {
			_ = root.Close()
		}
		return nil, errors.Join(openErr, closeErr)
	}
	return root, nil
}

// GenerationMarkerInfo is the inaccessible private half of this generation's
// validated marker, not an inode number recovered from the public pathname.
func GenerationMarkerInfo(repo string, identity Identity) (os.FileInfo, error) {
	record, err := readGenerationRecord(repo, identity.Name)
	if err != nil || record.Generation != identity.Generation {
		return nil, errors.Join(errors.New("fork generation changed"), err)
	}
	state, err := OpenStateRoot(repo, "", false)
	if err != nil {
		return nil, err
	}
	info, statErr := fsidentity.MarkerInfo(forkGenerationBinding(repo, record, state))
	return info, errors.Join(statErr, state.Close())
}

// ResolveProjectBinding maps a command launched directly from a fork checkout back to its
// canonical project. The path convention alone is never authority: a matching host generation
// record must validate the exact anchored workspace before the binding is returned.
func ResolveProjectBinding(repo string) (string, *Identity, error) {
	repo = filepath.Clean(repo)
	parent := filepath.Dir(repo)
	if !filepath.IsAbs(repo) || !strings.HasSuffix(parent, "-forks") {
		return repo, nil, nil
	}
	authorityRepo := strings.TrimSuffix(parent, "-forks")
	name := filepath.Base(repo)
	if authorityRepo == "" || !filepath.IsAbs(authorityRepo) || !ValidExistingName(name) {
		return repo, nil, nil
	}
	identity, ok, err := ReadGeneration(authorityRepo, name)
	if err != nil {
		return "", nil, fmt.Errorf("read canonical fork binding: %w", err)
	}
	if !ok {
		return repo, nil, nil
	}
	if Workspace(authorityRepo, name) != repo {
		return "", nil, errors.New("canonical fork binding points at another workspace")
	}
	if err := ValidateGenerationWorkspace(authorityRepo, identity); err != nil {
		return "", nil, fmt.Errorf("validate canonical fork binding: %w", err)
	}
	return authorityRepo, &identity, nil
}

// EnsureGenerationLocked anchors a new workspace or returns the identity already bound to it. The caller holds LockState(repo,name).
// A pidfile without a generation is deliberately not adopted because it may name a live old worker.
func EnsureGenerationLocked(repo, name string) (Identity, error) {
	return ensureGenerationLocked(repo, name, false)
}

// EnsureIsolatedGenerationLocked binds only a newly independently cloned workspace. Existing
// ordinary generations cannot be upgraded by assertion. The caller holds the lifecycle lock.
func EnsureIsolatedGenerationLocked(repo, name string) (Identity, error) {
	return ensureGenerationLocked(repo, name, true)
}

func ensureGenerationLocked(repo, name string, isolated bool) (Identity, error) {
	record, err := readGenerationRecord(repo, name)
	if err == nil {
		identity := Identity{Name: record.Name, Generation: record.Generation}
		if isolated && !record.Isolated {
			return Identity{}, fmt.Errorf("fork %s is not isolated — create a new fork with --isolated; its existing work is unchanged", name)
		}
		if err := ValidateGenerationWorkspace(repo, identity); err != nil {
			return Identity{}, err
		}
		return identity, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, err
	}
	if isolated {
		if err := ValidateIndependentGit(Workspace(repo, name)); err != nil {
			return Identity{}, fmt.Errorf("qualify isolated fork: %w", err)
		}
	}
	if _, err := os.Lstat(PidPath(repo, name)); err == nil {
		return Identity{}, fmt.Errorf("fork %s has legacy worker state without a generation — stop it, then retry", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, err
	}
	// A missing generation record does not make the human name free. Session teardown can
	// durably rename the old workspace first and retire its generation before retiring the
	// reservation. Fence both that staged tree and every old-generation owner before recovering or
	// minting authority for a replacement workspace.
	if err := RequireForkNameAvailable(repo, name); err != nil {
		return Identity{}, err
	}
	// Setup normally installs these before it returns, but an interrupted clone can be adopted by
	// a persisted session-create retry before that final step. Normalize both exclusions before
	// recovering or creating the marker, or Coop's own authority appears as user work and the model
	// can delete or commit it.
	for _, pattern := range []string{".coop/", "/" + GenerationMarkerName} {
		if err := ExcludeIfRepository(Workspace(repo, name), pattern); err != nil {
			return Identity{}, fmt.Errorf("exclude fork bookkeeping before binding generation: %w", err)
		}
	}
	state, err := OpenStateRoot(repo, "", true)
	if err != nil {
		return Identity{}, err
	}
	defer state.Close()
	if recovered, ok, err := recoverUnrecordedGeneration(repo, name, state); err != nil {
		return Identity{}, err
	} else if ok {
		if isolated {
			qualified, err := IsolatedGeneration(repo, recovered)
			if err != nil || !qualified {
				return Identity{}, errors.Join(errors.New("recovered fork is not isolated; existing work is unchanged"), err)
			}
		}
		return recovered, nil
	}
	generation := NewGeneration()
	record = generationRecord{
		Version: forkGenerationVersion, Name: name, Generation: generation, CreatedAt: time.Now().UTC(),
		WorkspaceAnchor: forkGenerationAnchorName(name, generation),
	}
	if isolated {
		record.Version, record.Isolated = forkGenerationIsolatedVersion, true
		record.CreationBase, err = gitOutputContext(context.Background(), Workspace(repo, name), "rev-parse", "HEAD")
		if err != nil || !ValidObjectID(record.CreationBase) {
			return Identity{}, errors.Join(errors.New("capture isolated fork creation base"), err)
		}
	}
	record.WorkspaceAnchor = generationAnchorName(record)
	if err := requireOneDisk(repo, record.Name); err != nil {
		return Identity{}, err
	}
	root, err := fsidentity.Create(forkGenerationBinding(repo, record, state))
	if err != nil {
		return Identity{}, fmt.Errorf("anchor fork workspace generation: %w", err)
	}
	if err := root.Close(); err != nil {
		_ = fsidentity.Retire(forkGenerationBinding(repo, record, state))
		return Identity{}, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		_ = fsidentity.Retire(forkGenerationBinding(repo, record, state))
		return Identity{}, err
	}
	published, err := writeGenerationAtomicResult(repo, name, append(data, '\n'))
	if err != nil {
		// A rename followed by a failed directory sync is uncertain to this process but already
		// visible to a retry. Keep the matching anchor so that published record remains recoverable.
		if !published {
			_ = fsidentity.Retire(forkGenerationBinding(repo, record, state))
		}
		return Identity{}, err
	}
	return Identity{Name: name, Generation: generation}, nil
}

func recoverUnrecordedGeneration(repo, name string, state *os.Root) (Identity, bool, error) {
	body, err := fsidentity.ReadMarker(Workspace(repo, name), GenerationMarkerName)
	if errors.Is(err, os.ErrNotExist) {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, fmt.Errorf("read interrupted fork identity marker: %w", err)
	}
	lines := strings.Split(string(body), "\n")
	if len(lines) < 4 || lines[1] != name || !ValidGeneration(Generation(lines[2])) ||
		!(lines[0] == "coop-fork-generation-v3" && len(lines) == 4 && lines[3] == "" ||
			lines[0] == "coop-fork-generation-v4-isolated" && len(lines) == 5 && ValidObjectID(lines[3]) && lines[4] == "") {
		return Identity{}, false, errors.New("reserved fork identity marker exists without a valid generation record")
	}
	generation := Generation(lines[2])
	record := generationRecord{Version: forkGenerationVersion, Name: name, Generation: generation,
		WorkspaceAnchor: forkGenerationAnchorName(name, generation), CreatedAt: time.Now().UTC()}
	if lines[0] == "coop-fork-generation-v4-isolated" {
		record.Version, record.Isolated = forkGenerationIsolatedVersion, true
		record.CreationBase = lines[3]
	}
	record.WorkspaceAnchor = generationAnchorName(record)
	root, err := fsidentity.Open(forkGenerationBinding(repo, record, state))
	if err != nil {
		return Identity{}, false, fmt.Errorf("interrupted fork identity cannot be recovered: %w", err)
	}
	if err := root.Close(); err != nil {
		return Identity{}, false, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return Identity{}, false, err
	}
	if err := writeGenerationAtomic(repo, name, append(data, '\n')); err != nil {
		return Identity{}, false, err
	}
	return Identity{Name: name, Generation: generation}, true, nil
}

func writeGenerationAtomic(repo, name string, data []byte) error {
	_, err := publishGenerationAtomic(repo, name, data)
	return err
}

func writeGenerationAtomicResult(repo, name string, data []byte) (bool, error) {
	return publishGenerationAtomic(repo, name, data)
}

// publishGenerationAtomic reports whether the final rename happened. An error after publication
// must not make a caller retire authority that the visible record still references.
func publishGenerationAtomic(repo, name string, data []byte) (bool, error) {
	state, err := OpenStateRoot(repo, "", true)
	if err != nil {
		return false, err
	}
	defer state.Close()
	f, tmp, err := CreateTempIn(state, "."+name+".generation-")
	if err != nil {
		return false, err
	}
	defer func() {
		if tmp != "" {
			_ = state.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	// Creation refuses an existing authority path: one fully-synced inode, published with a single rename.
	if err := PublishNoReplace(state, tmp, filepath.Base(GenerationPath(repo, name))); err != nil {
		return false, err
	}
	tmp = ""
	return true, nil
}

// RemoveGenerationIfMatchesLocked fences fresh/rm cleanup. A stale command may never erase the
// generation record of a replacement fork with the same human name. The record stays until its
// private anchor is retired: after a crash, it is the durable name a later retry needs to finish
// that cleanup.
func RemoveGenerationIfMatchesLocked(repo string, expected Identity) error {
	record, err := readGenerationRecord(repo, expected.Name)
	if errors.Is(err, os.ErrNotExist) {
		if err := retireGenerationAnchor(repo, generationRecord{Version: forkGenerationVersion,
			Name: expected.Name, Generation: expected.Generation,
			WorkspaceAnchor: forkGenerationAnchorName(expected.Name, expected.Generation), CreatedAt: time.Unix(1, 0)}); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if record.Name != expected.Name || record.Generation != expected.Generation {
		return errors.New("fork generation changed before removal")
	}
	if record.Version == forkGenerationVersion || record.Version == forkGenerationIsolatedVersion {
		if err := retireGenerationAnchor(repo, record); err != nil {
			return err
		}
	}
	if state, err := OpenStateRoot(repo, "", false); err == nil {
		removeErr := state.Remove(filepath.Base(GenerationPath(repo, expected.Name)))
		closeErr := state.Close()
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
		if closeErr != nil {
			return closeErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func retireGenerationAnchor(repo string, record generationRecord) error {
	state, err := OpenStateRoot(repo, "", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer state.Close()
	return fsidentity.Retire(forkGenerationBinding(repo, record, state))
}
