package networkstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// projectBuildRecord approves the immutable image a project build produced from these inputs. A
// restricted launch builds on a miss only files a person already approved (projectBuildReviewRecord).
// Only the current version is human authority: any other cannot authorize reuse.
//
// There is one record per output tag — one project on one locked client image — so a tree that
// keeps changing (a loop commits every iteration) replaces its record instead of accumulating them.
type projectBuildRecord struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Tag     string `json:"tag"`
	Inputs  string `json:"inputs"`
	Image   string `json:"image"`
}

// projectBuildID names a record by its tag. Like every record name here it is a plain hash: what
// keeps a planted record out is that only coop writes the store, which no box can reach.
func (s *Store) projectBuildID(tag string) (string, error) {
	if !safeRecordToken(tag, 256) {
		return "", errors.New("invalid project build record identity")
	}
	sum := sha256.Sum256([]byte("project-build-v2\x00" + tag))
	return hex.EncodeToString(sum[:]), nil
}

func projectBuildRecordName(id string) string { return "projectbuild-" + id + ".json" }

// ApprovedProjectBuild returns the image built from exactly these approved inputs, or "" when
// approval is missing, unreadable, foreign, legacy or for other inputs.
func (s *Store) ApprovedProjectBuild(tag, inputs string) string {
	image, _ := s.ProjectBuildStatus(tag, inputs)
	return image
}

// ProjectBuildStatus explains a readiness miss without granting build or launch authority.
// The immutable image still needs the caller's existence and derivation/client proofs.
func (s *Store) ProjectBuildStatus(tag, inputs string) (image, reason string) {
	if s.intactAuthority() != nil || !lowerHex(inputs, 64) {
		return "", "the saved project build approval is unavailable"
	}
	id, err := s.projectBuildID(tag)
	if err != nil {
		return "", "the saved project build approval is unavailable"
	}
	data, err := s.read(projectBuildRecordName(id))
	if err != nil {
		return "", "no explicit project build is recorded for this client image"
	}
	var record projectBuildRecord
	if json.Unmarshal(data, &record) != nil || record.Version != 2 || record.ID != id || record.Tag != tag || !imageDigest(record.Image) {
		return "", "the saved project build approval is no longer usable"
	}
	if record.Inputs != inputs {
		return "", "project build inputs or settings changed since the explicit build"
	}
	return record.Image, ""
}

// ApproveProjectBuild records a project build after its proofs, replacing the tag's previous approval.
// Callers must report publication failure: an image built without this record is not ready.
func (s *Store) ApproveProjectBuild(tag, inputs, image string) error {
	if err := s.intactAuthority(); err != nil {
		return err
	}
	id, err := s.projectBuildID(tag)
	if err != nil {
		return err
	}
	if !lowerHex(inputs, 64) || !imageDigest(image) {
		return errors.New("a project build record needs an inputs digest and an image id")
	}
	data, err := json.Marshal(projectBuildRecord{Version: 2, ID: id, Tag: tag, Inputs: inputs, Image: image})
	if err != nil {
		return err
	}
	if err := s.publish(projectBuildRecordName(id), data, true); err != nil {
		return err
	}
	// A build that passed supersedes the tag's last refusal. A refusal left behind only explains, so
	// failing to remove it is no reason to fail the approval.
	_ = s.removeRecord(projectBuildRefusalName(id))
	return nil
}

// projectBuildRefusalRecord remembers why Coop refused the image the last build of a tag's inputs
// made, so a launch on those same inputs repeats the reason and its fix instead of asking for a build
// that would be refused again. It approves nothing: it is only ever read as an explanation.
type projectBuildRefusalRecord struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Tag     string `json:"tag"`
	Inputs  string `json:"inputs"`
	Reason  string `json:"reason"`
}

func projectBuildRefusalName(id string) string { return "projectrefusal-" + id + ".json" }

// maxProjectBuildRefusal bounds a saved reason: one refusal, never a build log.
const maxProjectBuildRefusal = 4 << 10

// RecordProjectBuildRefusal remembers why Coop refused the image these inputs built, replacing the
// tag's last refusal.
func (s *Store) RecordProjectBuildRefusal(tag, inputs, reason string) error {
	if err := s.intactAuthority(); err != nil {
		return err
	}
	id, err := s.projectBuildID(tag)
	if err != nil {
		return err
	}
	if !lowerHex(inputs, 64) || reason == "" || len(reason) > maxProjectBuildRefusal {
		return errors.New("a project build refusal needs an inputs digest and a short reason")
	}
	data, err := json.Marshal(projectBuildRefusalRecord{Version: 1, ID: id, Tag: tag, Inputs: inputs, Reason: reason})
	if err != nil {
		return err
	}
	return s.publish(projectBuildRefusalName(id), data, true)
}

// ProjectBuildRefusal returns why Coop refused the image the last build of exactly these inputs made,
// or "" when it refused no build of them.
func (s *Store) ProjectBuildRefusal(tag, inputs string) string {
	if s.intactAuthority() != nil || !lowerHex(inputs, 64) {
		return ""
	}
	id, err := s.projectBuildID(tag)
	if err != nil {
		return ""
	}
	data, err := s.read(projectBuildRefusalName(id))
	if err != nil {
		return ""
	}
	var record projectBuildRefusalRecord
	if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.ID != id || record.Tag != tag ||
		record.Inputs != inputs || len(record.Reason) > maxProjectBuildRefusal {
		return ""
	}
	return record.Reason
}

// projectBuildLockTry is how long a build waits for the project's lock before saying it is waiting.
const projectBuildLockTry = 200 * time.Millisecond

// projectBuildReviewRecord holds the build a person last approved for one project: the digest of its
// files, Dockerfile path, daemon and build settings. It is what lets a launch rebuild those same files
// by itself when only Coop's client image changed or the image is gone. An automatic rebuild runs only
// after matching it, so it can never add files a person has not seen.
type projectBuildReviewRecord struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Project string `json:"project"`
	Review  string `json:"review"`
	// Files is what the review covered, each staged path's own digest, so a refusal can name the
	// files that changed. It explains and approves nothing: Review alone is the approval.
	Files map[string]string `json:"files,omitempty"`
}

// maxReviewedFilesBytes bounds a review record that lists its files. A context too big to list keeps
// its approval, and a refusal then names no file.
const maxReviewedFilesBytes = 2 << 20

func projectBuildReviewID(project string) string {
	sum := sha256.Sum256([]byte("project-build-review-v1\x00" + project))
	return hex.EncodeToString(sum[:])
}

func projectBuildReviewName(id string) string { return "projectreview-" + id + ".json" }

// ProjectBuildReviewed reports whether review is what a person last approved for project. Anything
// missing, unreadable or foreign is no: a miss waits for a person, so it is always the safe answer.
func (s *Store) ProjectBuildReviewed(project, review string) bool {
	if s.intactAuthority() != nil || !filepath.IsAbs(project) || !lowerHex(review, 64) {
		return false
	}
	id := projectBuildReviewID(project)
	data, err := s.read(projectBuildReviewName(id))
	if err != nil {
		return false
	}
	// Every launch asks, so the file list a record may carry is skipped, not decoded.
	var record struct {
		Version int    `json:"version"`
		ID      string `json:"id"`
		Project string `json:"project"`
		Review  string `json:"review"`
	}
	if json.Unmarshal(data, &record) != nil {
		return false
	}
	return record.Version == 1 && record.ID == id && record.Project == project && record.Review == review
}

// RecordProjectBuildReview remembers what a build just approved for project, and the files it covered,
// replacing what was approved before. Callers must report a failure: without this record the next
// client image waits for a person again.
func (s *Store) RecordProjectBuildReview(project, review string, files map[string]string) error {
	if err := s.intactAuthority(); err != nil {
		return err
	}
	if !filepath.IsAbs(project) || !lowerHex(review, 64) {
		return errors.New("a project build review needs a project directory and a review digest")
	}
	id := projectBuildReviewID(project)
	record := projectBuildReviewRecord{Version: 1, ID: id, Project: project, Review: review, Files: files}
	data, err := json.Marshal(record)
	if err == nil && len(data) > maxReviewedFilesBytes {
		record.Files = nil
		data, err = json.Marshal(record)
	}
	if err != nil {
		return err
	}
	return s.publish(projectBuildReviewName(id), data, true)
}

// ProjectBuildReviewedFiles returns the files the last approved build of project covered, each path's
// digest, or false when no approval lists them: none was made, or it came from an older build or a
// context too big to list.
func (s *Store) ProjectBuildReviewedFiles(project string) (map[string]string, bool) {
	if s.intactAuthority() != nil || !filepath.IsAbs(project) {
		return nil, false
	}
	id := projectBuildReviewID(project)
	data, err := s.read(projectBuildReviewName(id))
	if err != nil {
		return nil, false
	}
	var record projectBuildReviewRecord
	if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.ID != id || record.Project != project || record.Files == nil {
		return nil, false
	}
	return record.Files, true
}

// ProjectBuildReviewStamp changes each time a build of project finishes, for a session waiting on one.
// It proves nothing: the session then asks the real question.
func (s *Store) ProjectBuildReviewStamp(project string) string {
	info, err := s.root.Stat(projectBuildReviewName(projectBuildReviewID(project)))
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d/%d", info.ModTime().UnixNano(), info.Size())
}

// LockProjectBuild runs one build of a project at a time across processes: two editor windows, a loop
// and a person's own `coop build` can all find the same image missing, and a project image can take
// many minutes to build. waiting, if set, is called once before a wait longer than a moment. The flock
// goes with this process. A build it was running can outlive a kill, in its own process group, and the
// next build then simply runs beside it; waiting has no bound of its own, since a person can interrupt.
func (s *Store) LockProjectBuild(ctx context.Context, project string, waiting func(), operation func() error) error {
	if ctx == nil {
		return errors.New("project build lock requires context")
	}
	if !filepath.IsAbs(project) {
		return errors.New("project build lock requires a project directory")
	}
	name := "projectbuild-lock-" + projectBuildReviewID(project)[:32]
	started := false
	run := func() error {
		started = true
		return operation()
	}
	attempt, cancel := context.WithTimeout(ctx, projectBuildLockTry)
	err := s.lockFile(attempt, name, run)
	cancel()
	if started || ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if waiting != nil {
		waiting()
	}
	return s.lockFile(ctx, name, run)
}
