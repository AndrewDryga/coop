package networkstate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"strings"
)

// ImageTree is a memoized digest of one directory archive in an immutable
// Docker image. It avoids copying the same client installation on every run.
type ImageTree struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type imageTreeRecord struct {
	Version int       `json:"version"`
	ID      string    `json:"id"`
	Image   string    `json:"image"`
	Root    string    `json:"root"`
	Tree    ImageTree `json:"tree"`
}

func (s *Store) imageTreeID(image, root string) (string, error) {
	if !safeRecordToken(image, 256) || !safeRecordToken(root, 4096) || !strings.HasPrefix(root, "/") || path.Clean(root) != root {
		return "", errors.New("invalid image tree record identity")
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("image-tree-v1\x00" + image + "\x00" + root))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func imageTreeRecordName(id string) string { return "imagetree-" + id + ".json" }

func (s *Store) ImageTreeDigest(image, root string) (ImageTree, bool) {
	if s.intactAuthority() != nil {
		return ImageTree{}, false
	}
	id, err := s.imageTreeID(image, root)
	if err != nil {
		return ImageTree{}, false
	}
	data, err := s.read(imageTreeRecordName(id), maxPrivateRecordBytes)
	if err != nil {
		return ImageTree{}, false
	}
	var record imageTreeRecord
	if strictJSON(data, &record) != nil || record.Version != 1 || record.ID != id || record.Image != image || record.Root != root || !validImageTree(record.Tree) {
		return ImageTree{}, false
	}
	return record.Tree, true
}

func (s *Store) RememberImageTree(image, root string, tree ImageTree) error {
	if err := s.intactAuthority(); err != nil {
		return err
	}
	id, err := s.imageTreeID(image, root)
	if err != nil {
		return err
	}
	if !validImageTree(tree) {
		return errors.New("an image tree record needs one readable digest")
	}
	data, err := json.Marshal(imageTreeRecord{Version: 1, ID: id, Image: image, Root: root, Tree: tree})
	if err != nil {
		return err
	}
	return s.publish(imageTreeRecordName(id), data, true)
}

func validImageTree(tree ImageTree) bool {
	return tree.Size > 0 && lowerHex(tree.SHA256, 64)
}
