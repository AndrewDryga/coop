package workerconnector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
	var saved publicationProgress
	if intent.Request.Mode != "existing_pull_request" {
		return saved, nil
	}
	path, digest, err := p.publicationProgressPath(intent)
	if err != nil {
		return saved, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return saved, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxPublicationProgressBytes {
		return saved, errors.New("invalid publication progress file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return saved, err
	}
	if len(data) > maxPublicationProgressBytes || json.Unmarshal(data, &saved) != nil || saved.Version != publicationProgressVersion || saved.IntentDigest != digest || !saved.Attempted || saved.Intent == nil || *saved.Intent != intent {
		return publicationProgress{}, errors.New("invalid publication progress")
	}
	if saved.Result != nil && !exactSavedPublicationResult(intent, *saved.Result) {
		return publicationProgress{}, errors.New("invalid saved publication result")
	}
	return saved, nil
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
	if r == nil || result.Conflict != nil || result.ErrorCode != "" ||
		r.Repository != intent.Repository.RepositoryRef || r.BranchRef != "refs/heads/"+req.Branch ||
		r.CandidateTree != req.CandidateTree || r.CommitSHA != req.CandidateHead || r.PullRequestNumber != req.PullRequestNumber ||
		r.PullRequestURL != fmt.Sprintf("https://github.com/%s/pull/%d", intent.Repository.GitHubRepository, req.PullRequestNumber) {
		return false
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
