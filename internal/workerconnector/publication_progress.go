package workerconnector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// This journal bridges Git's write and the service's completed operation.
// It never stores credentials or turns an uncertain attempt into permission.
// JSON can expand a valid 32 KiB body sixfold; reserve room for its receipt too.
const maxPublicationProgressBytes = 512 << 10
const publicationProgressVersion = 1

type publicationProgress struct {
	Version      int                        `json:"version"`
	IntentDigest string                     `json:"intent_digest"`
	Intent       *workerproto.PublishIntent `json:"intent"`
	Attempted    bool                       `json:"attempted"`
	Result       *workerproto.PublishResult `json:"result,omitempty"`
}

func (p publicationHost) publicationProgressPath(intent workerproto.PublishIntent) (string, string, error) {
	data, err := json.Marshal(intent)
	if err != nil {
		return "", "", err
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	directory := filepath.Join(p.stateRoot, "publication-results")
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", "", err
	}
	if err := requirePrivateDirectory(directory); err != nil {
		return "", "", err
	}
	if err := syncPublicationDirectory(p.stateRoot); err != nil {
		return "", "", err
	}
	return filepath.Join(directory, digest+".json"), digest, nil
}

func (p publicationHost) readPublicationProgress(intent workerproto.PublishIntent) (publicationProgress, error) {
	path, digest, err := p.publicationProgressPath(intent)
	if err != nil {
		return publicationProgress{}, err
	}
	saved, err := loadPublicationProgress(path, digest)
	if errors.Is(err, os.ErrNotExist) {
		return publicationProgress{}, nil
	}
	if err != nil {
		return publicationProgress{}, err
	}
	if *saved.Intent != intent {
		return publicationProgress{}, errors.New("invalid publication progress")
	}
	if saved.Result != nil && !exactSavedPublicationResult(intent, *saved.Result) {
		return publicationProgress{}, errors.New("invalid saved publication result")
	}
	return saved, nil
}

// loadPublicationProgress reads one private journal file whose saved intent hashes to its name.
func loadPublicationProgress(path, digest string) (publicationProgress, error) {
	var saved publicationProgress
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return saved, err
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxPublicationProgressBytes {
		return saved, errors.New("invalid publication progress file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return saved, err
	}
	if len(data) > maxPublicationProgressBytes || json.Unmarshal(data, &saved) != nil || saved.Version != publicationProgressVersion ||
		saved.IntentDigest != digest || !saved.Attempted || saved.Intent == nil {
		return publicationProgress{}, errors.New("invalid publication progress")
	}
	intent, err := json.Marshal(*saved.Intent)
	if hash := sha256.Sum256(intent); err != nil || hex.EncodeToString(hash[:]) != digest {
		return publicationProgress{}, errors.New("invalid publication progress")
	}
	return saved, nil
}

// publicationProgressName reads a journal folder entry's name: "<digest>.json" is a journal file,
// ".<digest>.json-<random>" is a temporary file of config.WriteFileAtomic.
func publicationProgressName(name string) (digest string, temporary, ok bool) {
	if rest, found := strings.CutPrefix(name, "."); found {
		digest, _, found = strings.Cut(rest, ".json-")
		return digest, true, found && validPublicationDigest(digest)
	}
	digest, found := strings.CutSuffix(name, ".json")
	return digest, false, found && validPublicationDigest(digest)
}

func validPublicationDigest(digest string) bool {
	return len(digest) == sha256.Size*2 && strings.Trim(digest, "0123456789abcdef") == ""
}

func (p publicationHost) savePublicationProgress(intent workerproto.PublishIntent, saved publicationProgress) error {
	// Service operation ownership serializes this intent. Still refuse to replace
	// confirmed evidence with a preparation marker if called by a later retry.
	previous, err := p.readPublicationProgress(intent)
	if err != nil {
		return err
	}
	if previous.Result != nil {
		return errors.New("publication is already confirmed")
	}
	path, digest, err := p.publicationProgressPath(intent)
	if err != nil {
		return err
	}
	saved.Version = publicationProgressVersion
	saved.IntentDigest = digest
	saved.Intent = &intent
	if saved.Result != nil && !exactSavedPublicationResult(intent, *saved.Result) {
		return errors.New("invalid publication result")
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	if len(data) > maxPublicationProgressBytes {
		return errors.New("publication progress exceeds its byte limit")
	}
	if err := config.WriteFileAtomic(path, data); err != nil {
		return err
	}
	// WriteFileAtomic syncs the file; sync the renamed entry before Git can run.
	return syncPublicationDirectory(filepath.Dir(path))
}

// publicationProgressRetention is the least time a journal file is kept, counted from its last write.
// The service's operation row is the lasting answer: rows are never deleted, and a settled one
// replays its saved result to every redelivery of its key without calling the publisher again, so
// once it settled no replay reads this file. The age margin covers a state folder put back from an
// older copy, in which that operation could be running again.
const publicationProgressRetention = 30 * 24 * time.Hour

// PrunePublicationProgress removes journal files older than publicationProgressRetention whose
// service operation settled reports final, and temporary files of an interrupted write that are as
// old (a live write is never 30 days old). It reads a file only once it is old enough. A journal
// file it cannot read or prove stays for good, as does one whose operation is unknown, still
// running or cannot be checked. It stops at the first error from settled.
func PrunePublicationProgress(stateRoot string, now time.Time, settled func(commandKey string) (bool, error)) (int, error) {
	directory := filepath.Join(stateRoot, "publication-results")
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err := requirePrivateDirectory(directory); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, err
	}
	removed := 0
	defer func() {
		if removed > 0 {
			_ = syncPublicationDirectory(directory)
		}
	}()
	for _, entry := range entries {
		digest, temporary, ok := publicationProgressName(entry.Name())
		if !ok {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < publicationProgressRetention {
			continue
		}
		if !temporary {
			saved, err := loadPublicationProgress(path, digest)
			if err != nil {
				continue
			}
			done, err := settled(saved.Intent.CommandKey)
			if err != nil {
				return removed, err
			}
			if !done {
				continue
			}
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func syncPublicationDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func exactSavedPublicationResult(intent workerproto.PublishIntent, result workerproto.PublishResult) bool {
	r := result.Receipt
	req := intent.Request
	if r == nil || result.Conflict != nil || result.ErrorCode != "" || result.ClosedPullRequest != nil ||
		r.Repository != intent.Repository.RepositoryRef || r.BranchRef != "refs/heads/"+req.Branch ||
		r.CandidateTree != req.CandidateTree || r.CommitSHA != req.CandidateHead || r.PullRequestNumber <= 0 ||
		(req.PullRequestNumber != 0 && r.PullRequestNumber != req.PullRequestNumber) ||
		r.PullRequestURL != fmt.Sprintf("https://github.com/%s/pull/%d", intent.Repository.GitHubRepository, r.PullRequestNumber) {
		return false
	}
	// New-PR mode opens its own PR and confirms it, so only published is final there.
	if req.Mode != "existing_pull_request" {
		return result.Status == "published" && r.TargetObservation == nil
	}
	if result.Status == "published" {
		return r.TargetObservation == nil
	}
	if r.TargetObservation == nil || r.TargetObservation.Validate() != nil {
		return false
	}
	return result.Status == "published_target_changed" && r.TargetObservation.State == "changed_after_push" ||
		result.Status == "published_target_unverified" && r.TargetObservation.State == "unverified_after_push"
}
