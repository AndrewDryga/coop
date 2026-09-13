package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

const (
	forkCandidateManifestVersion = 2
	forkCandidateRoundVersion    = 1
	forkCandidateRoundLimit      = uint64(1<<31 - 1)

	forkCandidateActive      = "active"
	forkCandidateSuperseding = "superseding"
	forkCandidateReviewing   = "reviewing"
	forkCandidatePublishing  = "publishing"
	forkCandidateLanded      = "landed"
	forkCandidateDiscarded   = "discarded"
)

type forkCandidateRef struct {
	Round uint64 `json:"round"`
	ID    string `json:"id"`
}

type forkCandidateRoundRecord struct {
	Version    int           `json:"version"`
	Round      uint64        `json:"round"`
	Candidate  ForkCandidate `json:"candidate"`
	Supersedes string        `json:"supersedes,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
}

// forkCandidateManifest is the one mutable pointer for a generation. Round records themselves are
// immutable. A pending candidate remains embedded until its immutable round exists, so a crash
// never loses the exact snapshot whose review or partial owner updates must be replayed.
type forkCandidateManifest struct {
	Version    int                `json:"version"`
	Fork       forkspace.Identity `json:"fork"`
	Phase      string             `json:"phase"`
	Current    *forkCandidateRef  `json:"current,omitempty"`
	Pending    *ForkCandidate     `json:"pending,omitempty"`
	TerminalID string             `json:"terminal_id,omitempty"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

type forkCandidateState struct {
	legacy   *ForkCandidate
	manifest *forkCandidateManifest
}

// ForkCandidateRoundStatus is the bounded operator-facing view of the current round transition.
// Exact candidate IDs remain authoritative; round numbers are display-only.
type ForkCandidateRoundStatus struct {
	Phase        string
	CurrentRound uint64
	CurrentID    string
	PendingRound uint64
	PendingID    string
	TerminalID   string
}

func forkCandidateHistoryRoot(repo string) string {
	return filepath.Join(forkspace.StateDir(repo), "candidate-history")
}

func forkCandidateHistoryDir(repo string, identity forkspace.Identity) string {
	return filepath.Join(forkCandidateHistoryRoot(repo), identity.Name+"."+string(identity.Generation))
}

func forkCandidateRoundName(ref forkCandidateRef) string {
	return fmt.Sprintf("%010d.%s.json", ref.Round, ref.ID)
}

func ensureForkCandidateHistoryDir(repo string, identity forkspace.Identity) error {
	if err := forkspace.EnsureStateDir(repo); err != nil {
		return err
	}
	if err := ensureRealDirectory(forkCandidateHistoryRoot(repo), 0o700); err != nil {
		return err
	}
	if err := ensureRealDirectory(forkCandidateHistoryDir(repo, identity), 0o700); err != nil {
		return err
	}
	for _, dirPath := range []string{forkspace.StateDir(repo), forkCandidateHistoryRoot(repo)} {
		dir, err := os.Open(dirPath)
		if err != nil {
			return err
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			return err
		}
	}
	return nil
}

func readForkCandidateControlFile(path, kind string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 ||
		info.Size() < 0 || info.Size() > forkCandidateLimit {
		return nil, false, fmt.Errorf("%s is not a bounded single-link regular file", kind)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		if err != nil {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("%s changed while opening", kind)
	}
	data, err := io.ReadAll(io.LimitReader(f, forkCandidateLimit+1))
	if err != nil || len(data) > forkCandidateLimit {
		if err != nil {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("%s exceeds its size limit", kind)
	}
	return data, true, nil
}

func decodeStrictCandidateJSON(data []byte, dst any, kind string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%s contains multiple JSON values", kind)
		}
		return err
	}
	return nil
}

func validateForkCandidateRef(ref forkCandidateRef) error {
	if ref.Round == 0 || ref.Round > forkCandidateRoundLimit || !validAssignmentID(ref.ID) {
		return errors.New("invalid fork candidate round reference")
	}
	return nil
}

func validateForkCandidateRound(record forkCandidateRoundRecord, identity forkspace.Identity) error {
	if record.Version != forkCandidateRoundVersion || record.Round == 0 ||
		record.Round > forkCandidateRoundLimit || record.CreatedAt.IsZero() ||
		!record.CreatedAt.Equal(record.Candidate.CreatedAt) || record.Candidate.Fork != identity {
		return errors.New("invalid fork candidate round")
	}
	if err := validateForkCandidate(record.Candidate); err != nil {
		return err
	}
	if record.Round == 1 {
		if record.Supersedes != "" {
			return errors.New("first fork candidate round supersedes another candidate")
		}
	} else if !validAssignmentID(record.Supersedes) {
		return errors.New("fork candidate round is missing its predecessor")
	}
	return nil
}

func validateForkCandidateManifest(manifest forkCandidateManifest, identity forkspace.Identity) error {
	if manifest.Version != forkCandidateManifestVersion || manifest.Fork != identity ||
		!forkspace.ValidExistingName(identity.Name) || !forkspace.ValidGeneration(identity.Generation) ||
		manifest.UpdatedAt.IsZero() {
		return errors.New("invalid fork candidate manifest")
	}
	if manifest.Current != nil {
		if err := validateForkCandidateRef(*manifest.Current); err != nil {
			return err
		}
	}
	switch manifest.Phase {
	case forkCandidateActive:
		if manifest.Current == nil || manifest.Pending != nil || manifest.TerminalID != "" {
			return errors.New("invalid active fork candidate manifest")
		}
	case forkCandidateSuperseding:
		if manifest.Current == nil || manifest.Pending != nil || manifest.TerminalID != "" || manifest.Current.Round == forkCandidateRoundLimit {
			return errors.New("invalid superseding fork candidate manifest")
		}
	case forkCandidateReviewing, forkCandidatePublishing:
		if manifest.Pending == nil || manifest.Pending.Fork != identity || manifest.TerminalID != "" {
			return errors.New("invalid pending fork candidate manifest")
		}
		if err := validateForkCandidate(*manifest.Pending); err != nil {
			return err
		}
		want := uint64(1)
		if manifest.Current != nil {
			if manifest.Current.Round == forkCandidateRoundLimit {
				return errors.New("fork candidate round limit reached")
			}
			want = manifest.Current.Round + 1
		}
		if pendingRound(manifest) != want {
			return errors.New("pending fork candidate has a non-contiguous round")
		}
	case forkCandidateLanded, forkCandidateDiscarded:
		if manifest.Pending != nil || !validAssignmentID(manifest.TerminalID) ||
			(manifest.Phase == forkCandidateLanded && (manifest.Current == nil || manifest.Current.ID != manifest.TerminalID)) {
			return errors.New("invalid terminal fork candidate manifest")
		}
	default:
		return errors.New("invalid fork candidate manifest phase")
	}
	return nil
}

func pendingRound(manifest forkCandidateManifest) uint64 {
	if manifest.Pending == nil {
		return 0
	}
	if manifest.Current == nil {
		return 1
	}
	return manifest.Current.Round + 1
}

func readForkCandidateRound(repo string, identity forkspace.Identity, ref forkCandidateRef) (forkCandidateRoundRecord, error) {
	if err := validateForkCandidateRef(ref); err != nil {
		return forkCandidateRoundRecord{}, err
	}
	root, err := os.OpenRoot(forkCandidateHistoryDir(repo, identity))
	if err != nil {
		return forkCandidateRoundRecord{}, err
	}
	defer root.Close()
	name := forkCandidateRoundName(ref)
	info, err := root.Lstat(name)
	if err != nil {
		return forkCandidateRoundRecord{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 ||
		info.Size() < 0 || info.Size() > forkCandidateLimit {
		return forkCandidateRoundRecord{}, errors.New("fork candidate round is not a bounded single-link regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return forkCandidateRoundRecord{}, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		if err != nil {
			return forkCandidateRoundRecord{}, err
		}
		return forkCandidateRoundRecord{}, errors.New("fork candidate round changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, forkCandidateLimit+1))
	if err != nil || len(data) > forkCandidateLimit {
		if err != nil {
			return forkCandidateRoundRecord{}, err
		}
		return forkCandidateRoundRecord{}, errors.New("fork candidate round exceeds its size limit")
	}
	var record forkCandidateRoundRecord
	if err := decodeStrictCandidateJSON(data, &record, "fork candidate round"); err != nil {
		return forkCandidateRoundRecord{}, err
	}
	if err := validateForkCandidateRound(record, identity); err != nil {
		return forkCandidateRoundRecord{}, err
	}
	if record.Round != ref.Round || record.Candidate.ID != ref.ID {
		return forkCandidateRoundRecord{}, errors.New("fork candidate round does not match its manifest reference")
	}
	return record, nil
}

func readForkCandidateState(repo string, identity forkspace.Identity) (forkCandidateState, bool, error) {
	data, exists, err := readForkCandidateControlFile(ForkCandidatePath(repo, identity), "fork candidate state")
	if err != nil || !exists {
		return forkCandidateState{}, exists, err
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return forkCandidateState{}, false, err
	}
	switch header.Version {
	case forkCandidateVersion:
		candidate, err := decodeForkCandidate(data)
		if err != nil {
			return forkCandidateState{}, false, err
		}
		if candidate.Fork != identity {
			return forkCandidateState{}, false, errors.New("fork candidate belongs to another generation")
		}
		return forkCandidateState{legacy: &candidate}, true, nil
	case forkCandidateManifestVersion:
		var manifest forkCandidateManifest
		if err := decodeStrictCandidateJSON(data, &manifest, "fork candidate manifest"); err != nil {
			return forkCandidateState{}, false, err
		}
		if err := validateForkCandidateManifest(manifest, identity); err != nil {
			return forkCandidateState{}, false, err
		}
		if manifest.Current != nil {
			current, err := readForkCandidateRound(repo, identity, *manifest.Current)
			if err != nil {
				return forkCandidateState{}, false, fmt.Errorf("read current fork candidate round: %w", err)
			}
			if current.Round > 1 {
				predecessor := forkCandidateRef{Round: current.Round - 1, ID: current.Supersedes}
				if _, err := readForkCandidateRound(repo, identity, predecessor); err != nil {
					return forkCandidateState{}, false, fmt.Errorf("read previous fork candidate round: %w", err)
				}
			}
		}
		if manifest.Phase == forkCandidatePublishing {
			ref := forkCandidateRef{Round: pendingRound(manifest), ID: manifest.Pending.ID}
			if record, err := readForkCandidateRound(repo, identity, ref); err == nil {
				supersedes := ""
				if manifest.Current != nil {
					supersedes = manifest.Current.ID
				}
				if !reflect.DeepEqual(record.Candidate, *manifest.Pending) || record.Supersedes != supersedes {
					return forkCandidateState{}, false, errors.New("published fork candidate round disagrees with its manifest")
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return forkCandidateState{}, false, err
			}
		}
		return forkCandidateState{manifest: &manifest}, true, nil
	default:
		return forkCandidateState{}, false, fmt.Errorf("unsupported fork candidate state version %d", header.Version)
	}
}

func writeForkCandidateRound(repo string, record forkCandidateRoundRecord) error {
	if err := validateForkCandidateRound(record, record.Candidate.Fork); err != nil {
		return err
	}
	if err := ensureForkCandidateHistoryDir(repo, record.Candidate.Fork); err != nil {
		return err
	}
	ref := forkCandidateRef{Round: record.Round, ID: record.Candidate.ID}
	if current, err := readForkCandidateRound(repo, record.Candidate.Fork, ref); err == nil {
		if reflect.DeepEqual(current, record) {
			return nil
		}
		return errors.New("fork candidate round already exists with different contents")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(forkCandidateHistoryDir(repo, record.Candidate.Fork))
	if err != nil {
		return err
	}
	defer root.Close()
	name := forkCandidateRoundName(ref)
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	writeErr := error(nil)
	if _, err := f.Write(append(body, '\n')); err != nil {
		writeErr = err
	} else {
		writeErr = f.Sync()
	}
	if err := f.Close(); err != nil {
		writeErr = errors.Join(writeErr, err)
	}
	if writeErr != nil {
		_ = root.Remove(name)
		return writeErr
	}
	dir, err := root.OpenFile(".", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func writeForkCandidateManifest(repo string, manifest forkCandidateManifest) error {
	if err := validateForkCandidateManifest(manifest, manifest.Fork); err != nil {
		return err
	}
	if err := forkspace.EnsureStateDir(repo); err != nil {
		return err
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(forkspace.StateDir(repo), "."+manifest.Fork.Name+".candidate-manifest-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, ForkCandidatePath(repo, manifest.Fork)); err != nil {
		return err
	}
	dir, err := os.Open(forkspace.StateDir(repo))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func candidateFromManifest(repo string, manifest forkCandidateManifest) (ForkCandidate, error) {
	if manifest.Current == nil {
		return ForkCandidate{}, errors.New("fork candidate manifest has no current round")
	}
	record, err := readForkCandidateRound(repo, manifest.Fork, *manifest.Current)
	if err != nil {
		return ForkCandidate{}, err
	}
	return record.Candidate, nil
}

func forkCandidateStateStatus(state forkCandidateState) ForkCandidateRoundStatus {
	if state.legacy != nil {
		return ForkCandidateRoundStatus{Phase: forkCandidateActive, CurrentRound: 1, CurrentID: state.legacy.ID}
	}
	manifest := *state.manifest
	status := ForkCandidateRoundStatus{Phase: manifest.Phase, TerminalID: manifest.TerminalID}
	if manifest.Current != nil {
		status.CurrentRound, status.CurrentID = manifest.Current.Round, manifest.Current.ID
	}
	if manifest.Pending != nil {
		status.PendingRound, status.PendingID = pendingRound(manifest), manifest.Pending.ID
	}
	return status
}

// ReadForkCandidateRoundStatus reads only the current manifest and its exact referenced rounds; it
// never scans passive history.
func ReadForkCandidateRoundStatus(repo string, identity forkspace.Identity) (ForkCandidateRoundStatus, bool, error) {
	state, exists, err := readForkCandidateState(repo, identity)
	if err != nil || !exists {
		return ForkCandidateRoundStatus{}, exists, err
	}
	return forkCandidateStateStatus(state), true, nil
}

func forkCandidateManifestActive(manifest forkCandidateManifest) bool {
	return manifest.Phase != forkCandidateLanded && manifest.Phase != forkCandidateDiscarded
}

type forkCandidateFingerprint struct {
	Legacy   *ForkCandidate            `json:"legacy,omitempty"`
	Manifest *forkCandidateManifest    `json:"manifest,omitempty"`
	Current  *forkCandidateRoundRecord `json:"current,omitempty"`
}

func forkCandidateFingerprintValue(repo string, identity forkspace.Identity) (forkCandidateFingerprint, bool, error) {
	state, exists, err := readForkCandidateState(repo, identity)
	if err != nil || !exists {
		return forkCandidateFingerprint{}, exists, err
	}
	value := forkCandidateFingerprint{Legacy: state.legacy, Manifest: state.manifest}
	if state.manifest != nil && state.manifest.Current != nil {
		record, err := readForkCandidateRound(repo, identity, *state.manifest.Current)
		if err != nil {
			return forkCandidateFingerprint{}, false, err
		}
		value.Current = &record
	}
	return value, true, nil
}

func pendingForkLandPath(repo string, identity forkspace.Identity) (bool, error) {
	_, err := os.Lstat(forkspace.LandIntentPath(repo, identity))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
