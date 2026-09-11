package forkspace

import (
	"bytes"
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
)

const (
	forkGenerationVersion = 1
	forkGenerationLimit   = 4096
	forkGenerationCount   = 4096
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

type generationRecord struct {
	Version         int        `json:"version"`
	Name            string     `json:"name"`
	Generation      Generation `json:"generation"`
	WorkspaceDevice uint64     `json:"workspace_device"`
	WorkspaceInode  uint64     `json:"workspace_inode"`
	CreatedAt       time.Time  `json:"created_at"`
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
	if record.Version != forkGenerationVersion || record.Name != name || !ValidExistingName(name) ||
		!ValidGeneration(record.Generation) || record.WorkspaceDevice == 0 ||
		record.WorkspaceInode == 0 || record.CreatedAt.IsZero() {
		return errors.New("invalid fork generation record")
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
	device, inode, err := workspaceGeneration(repo, identity.Name)
	if err != nil {
		return err
	}
	if device != record.WorkspaceDevice || inode != record.WorkspaceInode {
		return errors.New("fork workspace no longer matches its generation")
	}
	return nil
}

// OpenGenerationWorkspaceRoot opens the exact workspace inode bound to identity. Callers that
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
	path := Workspace(repo, identity.Name)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 ||
		uint64(stat.Dev) != record.WorkspaceDevice || uint64(stat.Ino) != record.WorkspaceInode {
		return nil, errors.New("fork workspace no longer matches its generation")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("fork workspace changed while opening its generation root")
	}
	return root, nil
}

// ResolveProjectBinding maps a command launched directly from a fork checkout back to its
// canonical project. The path convention alone is never authority: a matching host generation
// record must validate the exact workspace inode before the binding is returned.
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

// EnsureGenerationLocked adopts a legacy, stopped workspace or returns the identity already bound
// to it. The caller must hold LockState(repo,name); a pidfile without a generation is deliberately
// not adopted because it may name a live/reserved old worker.
func EnsureGenerationLocked(repo, name string) (Identity, error) {
	if identity, ok, err := ReadGeneration(repo, name); err != nil {
		return Identity{}, err
	} else if ok {
		if err := ValidateGenerationWorkspace(repo, identity); err != nil {
			return Identity{}, err
		}
		return identity, nil
	}
	if _, err := os.Lstat(PidPath(repo, name)); err == nil {
		return Identity{}, fmt.Errorf("fork %s has legacy worker state without a generation — stop it, then retry", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, err
	}
	device, inode, err := workspaceGeneration(repo, name)
	if err != nil {
		return Identity{}, err
	}
	generation, err := NewGeneration()
	if err != nil {
		return Identity{}, err
	}
	record := generationRecord{
		Version: forkGenerationVersion, Name: name, Generation: generation,
		WorkspaceDevice: device, WorkspaceInode: inode, CreatedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return Identity{}, err
	}
	if err := writeGenerationAtomic(repo, name, append(data, '\n')); err != nil {
		return Identity{}, err
	}
	return Identity{Name: name, Generation: generation}, nil
}

func writeGenerationAtomic(repo, name string, data []byte) error {
	if err := EnsureStateDir(repo); err != nil {
		return err
	}
	f, err := os.CreateTemp(StateDir(repo), "."+name+".generation-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Publish the fully-synced inode only when the authority path is still absent. The platform
	// no-replace rename is a single-directory atomic operation and, unlike link+unlink, cannot leave
	// the immutable authority record with two links after a crash.
	if err := renameNoReplace(tmp, GenerationPath(repo, name)); err != nil {
		return err
	}
	tmp = ""
	dir, err := os.Open(StateDir(repo))
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	return errors.Join(syncErr, dir.Close())
}

// RemoveGenerationIfMatchesLocked fences fresh/rm cleanup. A stale command may never erase the
// generation record of a replacement fork with the same human name.
func RemoveGenerationIfMatchesLocked(repo string, expected Identity) error {
	current, ok, err := ReadGeneration(repo, expected.Name)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if current != expected {
		return errors.New("fork generation changed before removal")
	}
	if err := os.Remove(GenerationPath(repo, expected.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
