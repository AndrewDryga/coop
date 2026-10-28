// Package networkstate keeps network authority and execution evidence outside
// every agent mount. A repository can request access, never publish an approval.
package networkstate

import (
	"bytes"
	"context"
	"crypto/hmac"
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

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/fsidentity"
	"github.com/AndrewDryga/coop/internal/randid"
)

const maxPrivateRecordBytes = 4 << 20

type Store struct {
	root *os.Root
	path string
	key  []byte
	// afterPublish, set only by tests, runs once a record is in place under its final name: where an
	// interruption would leave it published and the steps after it undone.
	afterPublish func(name string) error
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
	// A first creating opener can leave the newly linked directory visible when its parent fsync
	// fails, before any owner key exists. Semantically that empty/temp-only directory is still no
	// authority store: read-only callers report absence, while the explicit approval path may retry
	// Open and repeat the creation barriers. loadKey already rejects any real record without a key.
	if s.key == nil {
		_ = s.Close()
		return nil, fmt.Errorf("network authority is not initialized: %w", os.ErrNotExist)
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
			overlaps, err := fsidentity.AliasesAncestor(pair[0], pair[1])
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
	current, err := s.read("owner.key")
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
		if create {
			if err := s.checkRootPath(); err != nil {
				return err
			}
		}
		s.key = key
		return nil
	}
	data, err := s.read("owner.key")
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
		if current, currentErr := s.read("owner.key"); currentErr == nil {
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
		key := randid.Bytes(32)
		if err := s.publish("owner.key", key, false); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		data, err = s.read("owner.key")
	}
	if err != nil {
		return err
	}
	return accept(data)
}

// removeRecord removes one of the store's own records and proves it is gone.
func (s *Store) removeRecord(name string) error {
	err := s.root.Remove(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := s.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("network record %s could not be proven removed", name)
	}
	return s.checkRootPath()
}

// read returns one of the store's own records. No box mounts the store (Open refuses one a mount
// could reach) and only coop writes it, so this is a plain read.
func (s *Store) read(name string) ([]byte, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return nil, errors.New("invalid network record name")
	}
	return s.root.ReadFile(name)
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
	tmp := ".publish-" + randid.Hex(16)
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
	if s.afterPublish != nil {
		if err := s.afterPublish(name); err != nil {
			return err
		}
	}
	return s.checkRootPath()
}

func (s *Store) projectID(project string) (string, error) {
	id, _, _, err := s.projectIdentity(project)
	return id, err
}

// projectIdentity resolves the one project identity an approval binds to: the
// canonical path, its id, and the directory the kernel has at that path
// right now. The path alone is a NAME — a directory moved aside and replaced by
// another at the same path would inherit its grants — so callers that authorize
// a launch compare the recorded inode too.
func (s *Store) projectIdentity(project string) (string, string, os.FileInfo, error) {
	canonical, err := canonicalPath(project)
	if err != nil {
		return "", "", nil, err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", "", nil, errors.New("network approval requires an existing project directory")
	}
	return s.projectKey(canonical), canonical, info, nil
}

// projectKey is the one derivation of a project's record id, so the path that
// files an approval and the path that finds it can never drift apart. It takes
// an already-canonical path: what makes an id is that path alone, so a record
// stays findable whatever happens to owner.key.
func (s *Store) projectKey(canonical string) string {
	sum := sha256.Sum256([]byte("project-v2\x00" + canonical))
	return hex.EncodeToString(sum[:])
}

func approvalRecord(id string) string { return "approval-" + id + ".json" }

// networkApprovalVersion is an approval filed under the project's id. Versions 1 to 3 are what
// builds up to v11.3.0 filed under the keyed id (legacy_records.go): none of them grants anything.
const (
	networkApprovalLegacyVersion   = 1
	networkApprovalBirthVersion    = 2
	networkApprovalAnchoredVersion = 3
	networkApprovalVersion         = 4
)

type Approval struct {
	Version   int           `json:"version"`
	ProjectID string        `json:"project_id"`
	Posture   egress.Mode   `json:"posture"`
	Envelope  []egress.Rule `json:"envelope"`
	// Device, Inode and Birth* are retained only to parse approvals written by
	// older Coop builds. They are allocator metadata, not current authority.
	Device    uint64 `json:"device,omitempty"`
	Inode     uint64 `json:"inode,omitempty"`
	BirthSec  int64  `json:"birth_sec,omitempty"`
	BirthNsec uint32 `json:"birth_nsec,omitempty"`
	// ProjectAnchor names the host-private half of a two-link filesystem anchor.
	// The repository marker must still be that exact inode for a launch to use
	// this approval.
	ProjectAnchor string `json:"project_anchor,omitempty"`
	// Services is the digest of each approved `service:` grant's Compose
	// definition, keyed by service name. A launch recomputes it from the file it
	// is about to run and refuses a service that changed since it was reviewed.
	Services map[string]string `json:"services,omitempty"`
	// Feature captures exclude automatically maintained core dependencies.
	Features []FeatureApproval `json:"features,omitempty"`
}

// checkDirectory refuses an approval whose project directory is no longer the
// one that was reviewed. An approval that never recorded that identity cannot
// prove it either, so it is reviewed again rather than trusted. Both are
// pending reviews, not corruption: the remedy is the same `coop approve`.
func (s *Store) checkDirectory(a *Approval, canonical string) *PendingApproval {
	if a == nil {
		if pending := s.legacyApprovalPending(canonical); pending != nil {
			return pending
		}
		// A moved checkout changes its path-keyed project id. Even an empty policy would otherwise
		// resolve to the fresh default and silently forget the old sticky posture. A marker with no
		// approval for this path is an explicit re-enrollment, not a first-use project.
		if _, err := fsidentity.ReadMarker(canonical, ProjectApprovalMarker); err == nil {
			return &PendingApproval{Reason: "this checkout moved or has a pending network identity — review it with 'coop approve'",
				Cause: "This checkout has a network approval marker but no approval for its current location."}
		} else if !errors.Is(err, os.ErrNotExist) {
			return &PendingApproval{Reason: "this checkout's network identity cannot be verified — review it with 'coop approve'",
				Cause: "Coop could not verify this checkout's network approval marker."}
		}
		return nil
	}
	if a.Version != networkApprovalVersion {
		return &PendingApproval{Reason: "the network approval for " + canonical + " was made by an older coop",
			Cause: "This project's network access was approved by an older Coop."}
	}
	if err := s.validateProjectAnchor(canonical, a); err != nil {
		return &PendingApproval{Reason: "the project directory at " + canonical + " was replaced since it was approved",
			Cause: "This project folder was replaced after its network access was approved."}
	}
	return nil
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
	data, err := s.read(approvalRecord(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var approval Approval
	if err := json.Unmarshal(data, &approval); err != nil {
		return nil, err
	}
	if approval.ProjectID != id || len(approval.Features) > egress.MaxFeatureExpansions {
		return nil, errors.New("network approval identity mismatch")
	}
	switch approval.Version {
	case networkApprovalLegacyVersion:
		if approval.Inode == 0 || approval.BirthSec != 0 || approval.BirthNsec != 0 || approval.ProjectAnchor != "" {
			return nil, errors.New("invalid legacy network approval identity")
		}
	case networkApprovalBirthVersion:
		if approval.Inode == 0 || approval.BirthSec == 0 || approval.ProjectAnchor != "" {
			return nil, errors.New("invalid birth-time network approval identity")
		}
	case networkApprovalAnchoredVersion, networkApprovalVersion:
		if approval.Device != 0 || approval.Inode != 0 || approval.BirthSec != 0 || approval.BirthNsec != 0 ||
			!projectAnchorRE.MatchString(approval.ProjectAnchor) {
			return nil, errors.New("invalid anchored network approval identity")
		}
	default:
		return nil, errors.New("unsupported network approval version")
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
	_, canonical, _, err := s.projectIdentity(project)
	if err != nil {
		return nil, err
	}
	if pending := s.checkDirectory(approval, canonical); pending != nil {
		return nil, pending
	}
	return s.checkRequests(approval, requests, bundles)
}

// checkRequests proves the request fits the approval. Its callers check the bundles first
// (checkBundles), each before its own other checks.
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
			return nil, errors.New("the optional provider features changed since they were approved — run 'coop approve' (network_approval_required)")
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
		return nil, errors.New("this project's egress rules were never approved — run 'coop approve' (network_approval_required)")
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
			return nil, errors.New("this project asks for more than what was approved — run 'coop approve' (network_approval_required)")
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
	if err := s.saveSnapshot(snapshot, true); err != nil {
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

// saveSnapshot publishes a capture once. removable marks one it creates for
// snapshot retention; a snapshot already there keeps whatever marking it has.
func (s *Store) saveSnapshot(snapshot egress.Snapshot, removable bool) error {
	if err := snapshot.Verify(s.key); err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	name := snapshotRecord(snapshot.Fingerprint)
	return s.lockSnapshots(context.Background(), func() error {
		err := s.publish(name, data, false)
		if err == nil && removable {
			return s.markAdmitted(snapshot.Fingerprint)
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, readErr := s.read(name)
		if readErr != nil || !bytes.Equal(existing, data) {
			return errors.New("network snapshot identity collision")
		}
		// An unchanged policy admitted again is in use again. Left at its first
		// admission, a project reopened after the retention window would have the
		// snapshot it just admitted removed before its run could load it.
		if err := s.touchAdmitted(snapshot.Fingerprint); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return s.checkRootPath()
	})
}

// LoadSnapshot is for a host-owned execution/session reference, never a field
// submitted by an agent. Existing runs retain their captured authority even if
// current approvals narrow; project binding prevents cross-project substitution.
func (s *Store) LoadSnapshot(project, fingerprint string) (egress.Snapshot, error) {
	id, err := s.projectID(project)
	if err != nil {
		return egress.Snapshot{}, err
	}
	return s.loadSnapshotScope(id, fingerprint)
}

func (s *Store) loadSnapshotScope(id, fingerprint string) (egress.Snapshot, error) {
	if len(fingerprint) != 64 {
		return egress.Snapshot{}, errors.New("invalid network snapshot reference")
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return egress.Snapshot{}, errors.New("invalid network snapshot reference")
	}
	// Loading is using: a run about to start under a removable snapshot, from a
	// process that admitted it long ago, restarts its retention window first, or
	// finds it removed. One without a marker is never removed, so its load takes
	// no lock and writes nothing. The exception, an earlier coop's setup check
	// snapshot, is marked only after its check's run, the one run that loads it,
	// ended 90 days ago.
	if _, err := s.root.Lstat(admittedRecord(fingerprint)); err == nil {
		if err := s.lockSnapshots(context.Background(), func() error { return s.touchAdmitted(fingerprint) }); err != nil {
			return egress.Snapshot{}, err
		}
	}
	data, err := s.read(snapshotRecord(fingerprint))
	if err != nil {
		return egress.Snapshot{}, err
	}
	var snapshot egress.Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return egress.Snapshot{}, err
	}
	if snapshot.Fingerprint != fingerprint || snapshot.Scope != id {
		return egress.Snapshot{}, errors.New("network snapshot reference mismatch")
	}
	return snapshot, snapshot.Verify(s.key)
}

func equalJSON(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}
