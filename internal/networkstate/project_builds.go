package networkstate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// projectBuildRecord approves the immutable image an explicit host build produced from these
// inputs. A restricted launch never builds on a miss. Version 1 was an automatic-build cache,
// not human authority, and cannot authorize reuse under this policy.
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

// projectBuildID keys a record by its tag with the owner key, like every other record name here: a
// process that cannot compute the name cannot plant one either.
func (s *Store) projectBuildID(tag string) (string, error) {
	if !safeRecordToken(tag, 256) {
		return "", errors.New("invalid project build record identity")
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("project-build-v1\x00" + tag))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func projectBuildRecordName(id string) string { return "projectbuild-" + id + ".json" }

// ApprovedProjectBuild returns an explicitly built image for exactly these inputs, or "" when
// approval is missing, unreadable, foreign, legacy or for other inputs.
func (s *Store) ApprovedProjectBuild(tag, inputs string) string {
	if s.intactAuthority() != nil || !lowerHex(inputs, 64) {
		return ""
	}
	id, err := s.projectBuildID(tag)
	if err != nil {
		return ""
	}
	data, err := s.read(projectBuildRecordName(id), maxPrivateRecordBytes)
	if err != nil {
		return ""
	}
	var record projectBuildRecord
	if strictJSON(data, &record) != nil || record.Version != 2 || record.ID != id || record.Tag != tag ||
		record.Inputs != inputs || !imageDigest(record.Image) {
		return ""
	}
	return record.Image
}

// ApproveProjectBuild records an explicit host build after its proofs, replacing the tag's previous
// approval. Callers must report publication failure: an image built without this record is not ready.
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
	return s.publish(projectBuildRecordName(id), data, true)
}
