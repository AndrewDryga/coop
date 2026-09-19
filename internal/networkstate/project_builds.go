package networkstate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// projectBuildRecord remembers the image one filtered project build produced and the digest of
// every input that build read. A launch whose inputs hash the same runs that exact image id instead
// of staging and building again. It is a memo, not authority: the launch still proves the image it
// runs, a record for other inputs is a miss, and a miss builds.
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

// ProjectBuild returns the image this host last built for tag from exactly these inputs, or "" when
// it remembers none: missing, unreadable, foreign or built from other inputs, each is a miss.
func (s *Store) ProjectBuild(tag, inputs string) string {
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
	if strictJSON(data, &record) != nil || record.Version != 1 || record.ID != id || record.Tag != tag ||
		record.Inputs != inputs || !imageDigest(record.Image) {
		return ""
	}
	return record.Image
}

// RememberProjectBuild records a build that just passed its proofs, replacing the tag's previous
// record. A write that fails costs the next launch a build, never a wrong image.
func (s *Store) RememberProjectBuild(tag, inputs, image string) error {
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
	data, err := json.Marshal(projectBuildRecord{Version: 1, ID: id, Tag: tag, Inputs: inputs, Image: image})
	if err != nil {
		return err
	}
	return s.publish(projectBuildRecordName(id), data, true)
}
