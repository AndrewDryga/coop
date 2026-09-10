// Package networkstate keeps network authority and execution evidence outside
// every agent mount. A repository can request access, never publish an approval.
package networkstate

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/AndrewDryga/coop/internal/egress"
)

const maxPrivateRecordBytes = 4 << 20

type Store struct {
	root *os.Root
	path string
	key  []byte
	// Injectable only inside this package to qualify rename-before-fsync faults.
	syncDir func(*os.File) error
}

// Open must be given all wholesale mount sources, including repo, companions,
// credential homes and any explicit extra mounts. Even an owner-only directory
// is not private authority when the same owner can reach it from the agent box.
func Open(path string, exposed []string) (*Store, error) {
	s, err := openFiles(path, exposed, true)
	if err != nil {
		return nil, err
	}
	if err := s.loadKey(true); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// OpenExisting restores owner authority without creating a directory or key.
// A replay cannot turn missing state into a new approval authority.
func OpenExisting(path string, exposed []string) (*Store, error) {
	s, err := openFiles(path, exposed, false)
	if err != nil {
		return nil, err
	}
	if err := s.loadKey(false); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.intactAuthority(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// The keyless result stays private to this package. Evidence recovery exposes
// a distinct capability and must never return an authority Store with no key.
func openFiles(path string, exposed []string, create bool) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("network state requires an absolute host path")
	}
	canonical, err := canonicalPath(path)
	if err != nil {
		return nil, err
	}
	if err := CheckPathExposure(canonical, exposed); err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(canonical, 0o700); err != nil {
			return nil, err
		}
		// Prospective descendants have no inode. Once the root exists, repeat
		// the identity check before publishing any key, including case or
		// normalization aliases that lexical comparisons could not prove.
		if err := CheckPathExposure(canonical, exposed); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		return nil, err
	}
	if err := privateInfo(info, true); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	return &Store{root: root, path: canonical}, nil
}

// CheckPathExposure protects even a not-yet-created authority tree without
// opening credentials or creating an owner key. Ordinary launches use this too.
func CheckPathExposure(path string, exposed []string) error {
	canonical, err := canonicalPath(path)
	if err != nil {
		return err
	}
	for _, mount := range exposed {
		if mount == "" {
			continue
		}
		source, err := canonicalPath(mount)
		if err != nil {
			return fmt.Errorf("network state mount isolation: %w", err)
		}
		if contains(source, canonical) || contains(canonical, source) {
			return errors.New("coop's network records must live outside every directory an agent can reach")
		}
		// Check both directions: the mount may be an alias of an authority
		// ancestor OR a file/future descendant below an aliased authority root.
		for _, pair := range [][2]string{{source, canonical}, {canonical, source}} {
			overlaps, err := aliasesAncestor(pair[0], pair[1])
			if err != nil {
				return fmt.Errorf("network state mount isolation: %w", err)
			}
			if overlaps {
				return errors.New("coop's network records must live outside every directory an agent can reach")
			}
		}
	}
	return nil
}

// canonicalPath resolves existing ancestors before appending missing components.
// Raw '..' is refused: cleaning it before resolving parent links changes meaning.
func canonicalPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("the path for coop's network records must be absolute")
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return "", errors.New("the path for coop's network records cannot contain a .. segment")
		}
	}
	probe := filepath.Clean(path)
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		tail = append(tail, filepath.Base(probe))
		probe = parent
	}
}

func contains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Path spelling is not identity on case-insensitive filesystems or through bind
// mounts. Compare each existing state ancestor with the actual exposed inode.
func aliasesAncestor(source, path string) (bool, error) {
	mount, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		// canonicalPath already resolved every existing ancestor. Lexical
		// overlap still rejects a future authority descendant; there is no
		// source inode to compare until composition creates the mount.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for {
		info, err := os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err == nil && os.SameFile(mount, info) {
			return true, nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false, nil
		}
		path = parent
	}
}

func privateInfo(info os.FileInfo, dir bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0o077 != 0 || info.Mode()&os.ModeSymlink != 0 || (dir && !info.IsDir()) || (!dir && !info.Mode().IsRegular()) {
		return errors.New("coop's network records must be plain files and directories only you can read")
	}
	return nil
}

func (s *Store) Close() error { return s.root.Close() }
func (s *Store) Path() string { return s.path }

// CheckExposure rechecks the actual mount sources after composition. Generated
// run-file descendants are validated separately against exact owned descriptors;
// this method never grants a prefix-based exception to authority isolation.
func (s *Store) CheckExposure(exposed []string) error {
	checked, err := openFiles(s.path, exposed, false)
	if err != nil {
		return err
	}
	defer checked.Close()
	return s.checkRootPath()
}

func (s *Store) authorityAvailable() error {
	current, err := s.read("owner.key", 32)
	if err != nil || len(s.key) != 32 || !hmac.Equal(current, s.key) {
		return errors.New("the key protecting coop's network records is missing or changed, so nothing new can be remembered")
	}
	return nil
}

// A key read through the anchored root alone cannot detect replacement or
// permission widening of the directory currently named by its host path.
func (s *Store) intactAuthority() error {
	if err := s.checkRootPath(); err != nil {
		return err
	}
	return s.authorityAvailable()
}

func (s *Store) loadKey(create bool) error {
	accept := func(key []byte) error {
		if len(key) != 32 {
			return errors.New("invalid network owner key")
		}
		// An existing key may have been linked by a first opener whose directory
		// sync failed. Every successful opener confirms that publication.
		if create {
			if err := s.confirmPublication(); err != nil {
				return err
			}
		}
		s.key = key
		return nil
	}
	data, err := s.read("owner.key", 32)
	if errors.Is(err, os.ErrNotExist) {
		// A lost key is not a fresh authority root. Refuse regeneration when any
		// durable records exist; otherwise a reset could erase remembered posture.
		entries, listErr := s.root.Open(".")
		if listErr != nil {
			return listErr
		}
		names, listErr := entries.Readdirnames(256)
		_ = entries.Close()
		if listErr != nil && !errors.Is(listErr, io.EOF) {
			return listErr
		}
		// Another first opener may have published the key while this one listed.
		if current, currentErr := s.read("owner.key", 32); currentErr == nil {
			return accept(current)
		} else if !errors.Is(currentErr, os.ErrNotExist) {
			return currentErr
		}
		if len(names) == 256 {
			return errors.New("network owner key missing: directory inspection exceeded its bound")
		}
		for _, name := range names {
			if !strings.HasPrefix(name, ".publish-") {
				return errors.New("network owner key missing from an existing authority store")
			}
		}
		if !create {
			return nil
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		if err := s.publish("owner.key", key, false); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		data, err = s.read("owner.key", 32)
	}
	if err != nil {
		return err
	}
	return accept(data)
}

func (s *Store) read(name string, limit int64) ([]byte, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return nil, errors.New("invalid network record name")
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	// Root.OpenFile resolves symlinks itself after ELOOP, including when its
	// caller supplies O_NOFOLLOW. Flat record names let openat enforce denial
	// relative to the already-open private directory without a path race.
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := privateInfo(info, false); err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, errors.New("network record exceeds byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("network record exceeds byte limit")
	}
	return data, nil
}

func (s *Store) publish(name string, data []byte, replace bool) error {
	return s.publishMode(name, data, replace, 0o600)
}

func (s *Store) publishMode(name string, data []byte, replace bool, mode os.FileMode) error {
	return s.publishBounded(name, data, replace, mode, maxPrivateRecordBytes)
}

func (s *Store) publishBounded(name string, data []byte, replace bool, mode os.FileMode, limit int) error {
	if len(data) > limit {
		return errors.New("network record exceeds byte limit")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	tmp := ".publish-" + hex.EncodeToString(random)
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer s.root.Remove(tmp)
	_, writeErr := f.Write(data)
	modeErr := f.Chmod(mode)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, modeErr, syncErr, closeErr); err != nil {
		return err
	}
	if replace {
		err = s.root.Rename(tmp, name)
	} else if mode == 0o444 {
		// A helper artifact requires one link for exact cleanup custody. A
		// link+deferred-unlink leaves two durable names after an abrupt crash.
		dir, openErr := s.root.Open(".")
		if openErr != nil {
			return openErr
		}
		err = renameExclusive(dir, tmp, name)
		_ = dir.Close()
	} else {
		err = s.root.Link(tmp, name)
	}
	if err != nil {
		return err
	}
	return s.confirmPublication()
}

// An existing equal file may be the result of a previous publication whose
// directory sync failed. Its bytes alone cannot establish durable publication.
func (s *Store) confirmPublication() error {
	if err := s.checkRootPath(); err != nil {
		return err
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return s.syncDirectory(dir)
}

func (s *Store) syncDirectory(dir *os.File) error {
	if s.syncDir != nil {
		return s.syncDir(dir)
	}
	return dir.Sync()
}

func (s *Store) projectID(project string) (string, error) {
	id, _, _, err := s.projectIdentity(project)
	return id, err
}

// projectIdentity resolves the one project identity an approval binds to: the
// canonical path, its keyed id, and the directory the kernel has at that path
// right now. The path alone is a NAME — a directory moved aside and replaced by
// another at the same path would inherit its grants — so callers that authorize
// a launch compare the recorded device/inode too.
func (s *Store) projectIdentity(project string) (string, string, os.FileInfo, error) {
	canonical, err := canonicalPath(project)
	if err != nil {
		return "", "", nil, err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", "", nil, errors.New("network approval requires an existing project directory")
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("project-v1\x00" + canonical))
	return hex.EncodeToString(mac.Sum(nil)), canonical, info, nil
}

type Approval struct {
	Version   int           `json:"version"`
	ProjectID string        `json:"project_id"`
	Posture   egress.Mode   `json:"posture"`
	Envelope  []egress.Rule `json:"envelope"`
	// Device and Inode are the approved directory's kernel identity. They are
	// recorded so a replacement at the same path cannot inherit its grants.
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	// Services is the digest of each approved `service:` grant's Compose
	// definition, keyed by service name. A launch recomputes it from the file it
	// is about to run and refuses a service that changed since it was reviewed.
	Services map[string]string `json:"services,omitempty"`
	// Feature captures exclude automatically maintained core dependencies.
	Features []FeatureApproval `json:"features,omitempty"`
}

// checkDirectory refuses an approval whose project directory is no longer the
// one that was reviewed. An approval that never recorded that identity cannot
// prove it either, so it is reviewed again rather than trusted.
func (a *Approval) checkDirectory(canonical string, info os.FileInfo) error {
	if a == nil {
		return nil
	}
	device, inode, ok := directoryIdentity(info)
	if !ok {
		return errors.New("the project directory at " + canonical + " could not be read")
	}
	if a.Inode == 0 {
		return errors.New("the approval for " + canonical + " was remembered by an older coop — review it with 'coop net approve'")
	}
	if a.Device != device || a.Inode != inode {
		return errors.New("the project directory at " + canonical + " was replaced since it was approved — review it with 'coop net approve'")
	}
	return nil
}

func directoryIdentity(info os.FileInfo) (uint64, uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return 0, 0, false
	}
	return uint64(stat.Dev), stat.Ino, true
}

type FeatureApproval = egress.FeatureExpansion

func (s *Store) Approval(project string) (*Approval, error) {
	id, err := s.projectID(project)
	if err != nil {
		return nil, err
	}
	return s.approval(id)
}

func (s *Store) approval(id string) (*Approval, error) {
	data, err := s.read("approval-"+id+".json", maxPrivateRecordBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var approval Approval
	if err := strictJSON(data, &approval); err != nil {
		return nil, err
	}
	if approval.Version != 1 || approval.ProjectID != id || len(approval.Features) > egress.MaxFeatureExpansions {
		return nil, errors.New("network approval identity mismatch")
	}
	if _, err := egress.ParseMode(string(approval.Posture)); err != nil {
		return nil, err
	}
	normalized, err := egress.CanonicalRules(approval.Envelope)
	if err != nil || !equalJSON(normalized, approval.Envelope) {
		return nil, errors.New("invalid network approval envelope")
	}
	if approval.Posture != egress.Filtered && (len(approval.Envelope) != 0 || len(approval.Features) != 0) {
		return nil, errors.New("network approval rules contradict posture")
	}
	services, err := approvedServices(approval.Envelope, approval.Services)
	if err != nil || !equalJSON(services, approval.Services) {
		return nil, errors.New("network approval service definitions do not match its rules")
	}
	return &approval, nil
}

func (s *Store) CheckRequests(project string, requests []egress.Rule, bundles []egress.Bundle) (*Approval, error) {
	if err := s.authorityAvailable(); err != nil {
		return nil, err
	}
	approval, err := s.Approval(project)
	if err != nil {
		return nil, err
	}
	if err := s.checkBundles(bundles); err != nil {
		return nil, err
	}
	return s.checkRequests(approval, requests, bundles)
}

// checkRequests proves the request fits the approval. The bundle content check is the CALLER's,
// because pinning a first-seen bundle is a write and not every caller may perform one.
func (s *Store) checkRequests(approval *Approval, requests []egress.Rule, bundles []egress.Bundle) (*Approval, error) {
	rules, err := checkRequestEnvelope(approval, requests)
	if err != nil {
		return nil, err
	}
	features, err := featureApprovals(rules, bundles)
	if err != nil {
		return nil, err
	}
	for _, feature := range features {
		found := false
		for _, approved := range approval.Features {
			if feature.Provider == approved.Provider && feature.Client == approved.Client && feature.Backend == approved.Backend &&
				feature.AuthMode == approved.AuthMode && feature.Feature == approved.Feature && equalJSON(feature.Rules, approved.Rules) {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("the optional provider features changed since they were approved — run 'coop net approve' (network_approval_required)")
		}
	}
	return approval, nil
}

// Preliminary envelope checks do not derive optional features from mutable
// provider inputs. Admit still validates their exact expansion atomically.
func checkRequestEnvelope(approval *Approval, requests []egress.Rule) ([]egress.Rule, error) {
	rules, err := egress.NormalizeRules(requests)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return rules, nil
	} // sticky posture survives YAML deletion.
	if approval == nil || approval.Posture != egress.Filtered {
		return nil, errors.New("this project's egress rules were never approved — run 'coop net approve' (network_approval_required)")
	}
	for _, request := range rules {
		found := false
		for _, approved := range approval.Envelope {
			if egress.Covers(approved, request) {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("this project asks for more than what was approved — run 'coop net approve' (network_approval_required)")
		}
	}
	return rules, nil
}

func featureApprovals(rules []egress.Rule, bundles []egress.Bundle) ([]FeatureApproval, error) {
	return egress.FeatureExpansions(rules, bundles)
}

func (s *Store) capture(project, id string, mode egress.Mode, requests []egress.Rule, operator []egress.Input, bundles []egress.Bundle, exportDestinations bool) (egress.Snapshot, error) {
	snapshot, err := s.compile(project, id, mode, requests, operator, bundles, exportDestinations)
	if err != nil {
		return egress.Snapshot{}, err
	}
	if err := s.saveSnapshot(snapshot); err != nil {
		return egress.Snapshot{}, err
	}
	return snapshot, nil
}

// compile is the authority itself and touches nothing on disk. Both a capture
// and a non-publishing Resolve end here, so the fingerprint one publishes is the
// fingerprint the other reports.
func (s *Store) compile(project, id string, mode egress.Mode, requests []egress.Rule, operator []egress.Input, bundles []egress.Bundle, exportDestinations bool) (egress.Snapshot, error) {
	if err := s.authorityAvailable(); err != nil {
		return egress.Snapshot{}, err
	}
	current, err := s.projectID(project)
	if err != nil || current != id {
		return egress.Snapshot{}, errors.New("network project identity changed during admission")
	}
	inputs := slices.Clone(operator)
	if len(requests) != 0 {
		inputs = append(inputs, egress.Input{Rules: requests, Origin: egress.Origin{Kind: "project", Name: id}})
	}
	return egress.Compile(id, mode, inputs, bundles, exportDestinations, s.key)
}

func (s *Store) saveSnapshot(snapshot egress.Snapshot) error {
	if err := snapshot.Verify(s.key); err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	name := "snapshot-" + snapshot.Fingerprint + ".json"
	if err := s.publish(name, data, false); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, readErr := s.read(name, maxPrivateRecordBytes)
		if readErr != nil || !bytes.Equal(existing, data) {
			return errors.New("network snapshot identity collision")
		}
		return s.confirmPublication()
	}
	return nil
}

// LoadSnapshot is for a host-owned execution/session reference, never a field
// submitted by an agent. Existing runs retain their captured authority even if
// current approvals narrow; project binding prevents cross-project substitution.
func (s *Store) LoadSnapshot(project, fingerprint string) (egress.Snapshot, error) {
	if len(fingerprint) != 64 {
		return egress.Snapshot{}, errors.New("invalid network snapshot reference")
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return egress.Snapshot{}, errors.New("invalid network snapshot reference")
	}
	id, err := s.projectID(project)
	if err != nil {
		return egress.Snapshot{}, err
	}
	data, err := s.read("snapshot-"+fingerprint+".json", maxPrivateRecordBytes)
	if err != nil {
		return egress.Snapshot{}, err
	}
	var snapshot egress.Snapshot
	if err := strictJSON(data, &snapshot); err != nil {
		return egress.Snapshot{}, err
	}
	if snapshot.Fingerprint != fingerprint || snapshot.Scope != id {
		return egress.Snapshot{}, errors.New("network snapshot reference mismatch")
	}
	return snapshot, snapshot.Verify(s.key)
}

func strictJSON(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("invalid network record: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("network record has trailing content")
	}
	return nil
}

func equalJSON(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}
