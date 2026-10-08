package forkspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
)

const (
	forkGenerationLegacyVersion   = 1
	forkGenerationBirthVersion    = 2
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

// Injectable only inside this package to qualify rename/unlink-before-directory-sync recovery.
var syncGenerationDirectory = func(dir *os.File) error { return dir.Sync() }

// Generation is an immutable incarnation of one named fork. A name can be reused after --fresh
// or rm; a generation cannot, so stale workers, boxes, task assignments, and candidates cannot
// silently attach themselves to the replacement workspace.
type Generation string

type Identity struct {
	Name       string     `json:"name"`
	Generation Generation `json:"generation"`
}

// generationRecord binds a fork's logical generation to one physical workspace. Versions 1 and 2
// are the historical inode and inode+birth-time formats; supported overlay filesystems can reuse
// both. Version 3 names a private hardlink anchor whose live inode is compared with the marker in
// the workspace on every authoritative open.
type generationRecord struct {
	Version            int        `json:"version"`
	Name               string     `json:"name"`
	Generation         Generation `json:"generation"`
	WorkspaceDevice    uint64     `json:"workspace_device,omitempty"`
	WorkspaceInode     uint64     `json:"workspace_inode,omitempty"`
	WorkspaceBirthSec  int64      `json:"workspace_birth_sec,omitempty"`
	WorkspaceBirthNsec uint32     `json:"workspace_birth_nsec,omitempty"`
	WorkspaceAnchor    string     `json:"workspace_anchor,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	Isolated           bool       `json:"isolated,omitempty"`
	CreationBase       string     `json:"creation_base,omitempty"`
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

func NewGeneration() (Generation, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return Generation(hex.EncodeToString(raw[:])), nil
}

func ValidGeneration(g Generation) bool { return forkGenerationRE.MatchString(string(g)) }

func forkGenerationAnchorName(_ string, generation Generation) string {
	// The random generation is already unique within this repository. Keeping the human fork name
	// out of the private filename preserves names that fit the historical .generation.json record
	// at NAME_MAX; the authenticated marker body still binds both name and generation.
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

func workspaceGeneration(repo, name string) (uint64, uint64, error) {
	path := Workspace(repo, name)
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, 0, fmt.Errorf("fork workspace %q is not a real directory", path)
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func validateGenerationRecord(record generationRecord, name string) error {
	if record.Name != name || !ValidExistingName(name) || !ValidGeneration(record.Generation) ||
		record.CreatedAt.IsZero() {
		return errors.New("invalid fork generation record")
	}
	if record.Isolated != (record.Version == forkGenerationIsolatedVersion) {
		return errors.New("fork isolation does not match its authority version")
	}
	if record.Isolated && !validPinnedCommit(record.CreationBase) || !record.Isolated && record.CreationBase != "" {
		return errors.New("fork creation base does not match its isolation authority")
	}
	switch record.Version {
	case forkGenerationLegacyVersion:
		if record.WorkspaceDevice == 0 || record.WorkspaceInode == 0 || record.WorkspaceBirthSec != 0 ||
			record.WorkspaceBirthNsec != 0 || record.WorkspaceAnchor != "" {
			return errors.New("invalid legacy fork generation record")
		}
	case forkGenerationBirthVersion:
		if record.WorkspaceDevice == 0 || record.WorkspaceInode == 0 || record.WorkspaceBirthSec == 0 ||
			record.WorkspaceAnchor != "" {
			return errors.New("invalid birth-time fork generation record")
		}
	case forkGenerationVersion, forkGenerationIsolatedVersion:
		if record.WorkspaceDevice != 0 || record.WorkspaceInode != 0 || record.WorkspaceBirthSec != 0 ||
			record.WorkspaceBirthNsec != 0 ||
			record.WorkspaceAnchor != generationAnchorName(record) {
			return errors.New("invalid anchored fork generation record")
		}
	default:
		return errors.New("unsupported fork generation record")
	}
	return nil
}

func readGenerationRecord(repo, name string) (generationRecord, error) {
	path := GenerationPath(repo, name)
	info, err := os.Lstat(path)
	if err != nil {
		return generationRecord{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 ||
		info.Size() < 0 || info.Size() > forkGenerationLimit {
		return generationRecord{}, fmt.Errorf("fork generation record %q is not a bounded single-link regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return generationRecord{}, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		if err != nil {
			return generationRecord{}, err
		}
		return generationRecord{}, errors.New("fork generation record changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, forkGenerationLimit+1))
	if err != nil {
		return generationRecord{}, err
	}
	if len(data) > forkGenerationLimit {
		return generationRecord{}, fmt.Errorf("fork generation record exceeds %d bytes", forkGenerationLimit)
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
	root, err := os.OpenRoot(StateDir(repo))
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
	if record.Version != forkGenerationVersion && record.Version != forkGenerationIsolatedVersion {
		return fmt.Errorf("fork %s uses an older workspace identity — stop it and retry so Coop can verify and anchor it", identity.Name)
	}
	state, err := os.OpenRoot(StateDir(repo))
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
	if record.Version != forkGenerationVersion && record.Version != forkGenerationIsolatedVersion {
		return nil, fmt.Errorf("fork %s uses an older workspace identity — stop it and retry so Coop can verify and anchor it", identity.Name)
	}
	state, err := os.OpenRoot(StateDir(repo))
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
	state, err := os.OpenRoot(StateDir(repo))
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

// EnsureGenerationLocked anchors a new workspace, migrates a semantically verified stopped legacy
// workspace, or returns the identity already bound to it. The caller holds LockState(repo,name).
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
		if record.Version == forkGenerationVersion || record.Version == forkGenerationIsolatedVersion {
			if err := ValidateGenerationWorkspace(repo, identity); err != nil {
				return Identity{}, err
			}
			// A prior publication may have renamed this complete record and then failed the
			// state-directory sync. Repeat the barrier on every successful observation so a
			// retry cannot report durable authority while that directory entry is still volatile.
			if err := syncGenerationStateDir(repo); err != nil {
				return Identity{}, err
			}
			return identity, nil
		}
		if err := migrateLegacyGenerationLocked(repo, record); err != nil {
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
	// Session-create recovery can adopt a clone that died before Setup returned. Establish the
	// workspace name durably in its fork-home parent before publishing a marker, anchor or record.
	if err := confirmForkDirectoryEntry(Workspace(repo, name)); err != nil {
		return Identity{}, fmt.Errorf("confirm fork workspace before binding generation: %w", err)
	}
	if err := EnsureStateDir(repo); err != nil {
		return Identity{}, err
	}
	state, err := os.OpenRoot(StateDir(repo))
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
	generation, err := NewGeneration()
	if err != nil {
		return Identity{}, err
	}
	record = generationRecord{
		Version: forkGenerationVersion, Name: name, Generation: generation, CreatedAt: time.Now().UTC(),
		WorkspaceAnchor: forkGenerationAnchorName(name, generation),
	}
	if isolated {
		record.Version, record.Isolated = forkGenerationIsolatedVersion, true
		record.CreationBase, err = gitOutputContext(context.Background(), Workspace(repo, name), "rev-parse", "HEAD")
		if err != nil || !validPinnedCommit(record.CreationBase) {
			return Identity{}, errors.Join(errors.New("capture isolated fork creation base"), err)
		}
	}
	record.WorkspaceAnchor = generationAnchorName(record)
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

func migrateLegacyGenerationLocked(repo string, record generationRecord) error {
	identity := Identity{Name: record.Name, Generation: record.Generation}
	if NeedsStop(repo, record.Name) {
		return fmt.Errorf("fork %s uses an older workspace identity — stop it and retry so Coop can verify and anchor it", record.Name)
	}
	if err := RequireNoForkExecutionsLocked(repo, identity); err != nil {
		return err
	}
	if err := RequireNoWorkspaceReservationLocked(repo, identity); err != nil {
		return err
	}
	if _, err := os.Lstat(LandIntentPath(repo, identity)); err == nil {
		return fmt.Errorf("fork %s has an interrupted land journal — finish or recover the merge before anchoring it", record.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, inode, err := workspaceGeneration(repo, record.Name)
	if err != nil {
		return err
	}
	if inode != record.WorkspaceInode {
		return errors.New("fork workspace no longer matches its legacy generation")
	}
	if err := verifyForkRepository(repo, record.Name); err != nil {
		return fmt.Errorf("fork %s uses an older workspace identity and could not be safely verified: %w", record.Name, err)
	}
	if err := EnsureStateDir(repo); err != nil {
		return err
	}
	state, err := os.OpenRoot(StateDir(repo))
	if err != nil {
		return err
	}
	defer state.Close()
	if err := Exclude(Workspace(repo, record.Name), "/"+GenerationMarkerName); err != nil {
		return fmt.Errorf("exclude fork identity marker: %w", err)
	}
	anchored := record
	anchored.Version = forkGenerationVersion
	anchored.WorkspaceDevice, anchored.WorkspaceInode = 0, 0
	anchored.WorkspaceBirthSec, anchored.WorkspaceBirthNsec = 0, 0
	anchored.WorkspaceAnchor = forkGenerationAnchorName(record.Name, record.Generation)
	binding := forkGenerationBinding(repo, anchored, state)
	root, openErr := fsidentity.Open(binding)
	if openErr != nil {
		markerMissing := rootNameMissing(Workspace(repo, record.Name), GenerationMarkerName)
		anchorMissing := rootNameMissing(StateDir(repo), anchored.WorkspaceAnchor)
		if markerMissing && !anchorMissing {
			// An interrupted Create can leave only its private recovery name after removing the
			// public marker. Retire checks the exact body and durable public absence before a
			// retry mints the same binding; a stray or changed private file remains a refusal.
			if err := fsidentity.Retire(binding); err != nil {
				return fmt.Errorf("retire interrupted legacy fork anchor: %w", err)
			}
			anchorMissing = true
		}
		if !markerMissing || !anchorMissing {
			return fmt.Errorf("legacy fork identity has incomplete anchor state; remove neither file and retry recovery: %w", openErr)
		}
		root, err = fsidentity.Create(binding)
		if err != nil {
			return fmt.Errorf("anchor legacy fork workspace: %w", err)
		}
	}
	if err := root.Close(); err != nil {
		return err
	}
	data, err := json.Marshal(anchored)
	if err != nil {
		return err
	}
	if err := replaceGenerationAtomic(repo, record.Name, append(data, '\n')); err != nil {
		return fmt.Errorf("publish anchored fork generation: %w", err)
	}
	return nil
}

func verifyForkRepository(repo, name string) error {
	workspace := Workspace(repo, name)
	branch, err := gitOutputContext(context.Background(), workspace, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != name {
		return fmt.Errorf("workspace branch is %q, want %q", branch, name)
	}
	origin, ok, err := gitConfigContext(context.Background(), workspace, "remote.origin.url")
	if err != nil || !ok || origin == "" {
		return errors.Join(errors.New("workspace has no verifiable origin"), err)
	}
	if !filepath.IsAbs(origin) {
		return errors.New("workspace origin is not an absolute local repository")
	}
	want, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return err
	}
	got, err := filepath.EvalSymlinks(origin)
	if err != nil || filepath.Clean(got) != filepath.Clean(want) {
		return errors.Join(errors.New("workspace origin is not the canonical project"), err)
	}
	return nil
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
			lines[0] == "coop-fork-generation-v4-isolated" && len(lines) == 5 && validPinnedCommit(lines[3]) && lines[4] == "") {
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

func rootNameMissing(root, name string) bool {
	_, err := os.Lstat(filepath.Join(root, name))
	return errors.Is(err, os.ErrNotExist)
}

func writeGenerationAtomic(repo, name string, data []byte) error {
	_, err := publishGenerationAtomic(repo, name, data, false)
	return err
}

func writeGenerationAtomicResult(repo, name string, data []byte) (bool, error) {
	return publishGenerationAtomic(repo, name, data, false)
}

func replaceGenerationAtomic(repo, name string, data []byte) error {
	_, err := publishGenerationAtomic(repo, name, data, true)
	return err
}

// publishGenerationAtomic reports whether the final rename happened. An error after publication
// must not make a caller retire authority that the visible record still references.
func publishGenerationAtomic(repo, name string, data []byte, replace bool) (bool, error) {
	if err := EnsureStateDir(repo); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(StateDir(repo), "."+name+".generation-")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
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
	// Creation refuses an existing authority path; a locked legacy upgrade deliberately replaces
	// its old record. Both publish one fully-synced inode with a single rename.
	var publishErr error
	if replace {
		publishErr = os.Rename(tmp, GenerationPath(repo, name))
	} else {
		publishErr = renameNoReplace(tmp, GenerationPath(repo, name))
	}
	if publishErr != nil {
		return false, publishErr
	}
	tmp = ""
	dir, err := os.Open(StateDir(repo))
	if err != nil {
		return true, err
	}
	return true, errors.Join(syncGenerationDirectory(dir), dir.Close())
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
		// A prior attempt may have unlinked the record and then failed its directory sync. Repeat
		// that durability barrier before declaring cleanup complete.
		return syncGenerationStateDir(repo)
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
	if err := os.Remove(GenerationPath(repo, expected.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncGenerationStateDir(repo)
}

func syncGenerationStateDir(repo string) error {
	dir, err := os.Open(StateDir(repo))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.Join(syncGenerationDirectory(dir), dir.Close())
}

func retireGenerationAnchor(repo string, record generationRecord) error {
	state, err := os.OpenRoot(StateDir(repo))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer state.Close()
	return fsidentity.Retire(forkGenerationBinding(repo, record, state))
}
