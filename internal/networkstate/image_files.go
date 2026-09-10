package networkstate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// maxImageFilePaths bounds one record's path set. A qualified closure pins a
// dozen entry points; a request anywhere near this is not one of them.
const maxImageFilePaths = 256

// ImageFile is one file's identity inside an image — exactly what an image proof
// compares between two images.
type ImageFile struct {
	Mode   int64  `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ImageFileRecord is what this host already read out of ONE image id, for ONE
// exact set of paths. A Docker image id is a content address, so the same id is
// the same bytes: remembering a read is not trusting a claim about an image, it
// is declining to copy the same few hundred megabytes back out for every
// process that asks.
//
// It is NOT authority and it can never make a file pass. A launch still proves
// the image it is about to run; this only supplies digests that proof would
// otherwise re-read, and a record that does not cover exactly these paths for
// exactly this image id is not used at all.
type ImageFileRecord struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Image   string `json:"image"`
	// Files is keyed by absolute path and holds one entry per path in the id.
	Files map[string]ImageFile `json:"files"`
}

// imageFilesID keys a record by the image AND the exact set of paths, so a
// release that pins one more entry point cannot be satisfied by the record
// written before it. Like every other record name here it is keyed with the
// owner key: a process that cannot compute the name cannot plant one either.
func (s *Store) imageFilesID(image string, paths []string) (string, []string, error) {
	invalid := errors.New("invalid image file record identity")
	if !safeRecordToken(image, 256) || len(paths) == 0 || len(paths) > maxImageFilePaths {
		return "", nil, invalid
	}
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("image-files-v1\x00" + image + "\x00"))
	for i, path := range sorted {
		if !safeRecordToken(path, 4096) || !strings.HasPrefix(path, "/") || (i > 0 && sorted[i-1] == path) {
			return "", nil, invalid
		}
		_, _ = mac.Write([]byte(path + "\x00"))
	}
	return hex.EncodeToString(mac.Sum(nil)), sorted, nil
}

func imageFilesRecord(id string) string { return "imagefiles-" + id + ".json" }

// ImageFileDigests returns what this host remembered for exactly this image and
// path set, or nil when it remembers nothing it can prove. Missing, unreadable,
// foreign, or short of one path — each of those is a MISS, and a miss means the
// caller reads the image. There is deliberately no error to mishandle: the only
// two answers are the digests of this exact image or nothing at all.
func (s *Store) ImageFileDigests(image string, paths []string) map[string]ImageFile {
	if s.intactAuthority() != nil {
		return nil
	}
	id, sorted, err := s.imageFilesID(image, paths)
	if err != nil {
		return nil
	}
	data, err := s.read(imageFilesRecord(id), maxPrivateRecordBytes)
	if err != nil {
		return nil
	}
	var record ImageFileRecord
	if strictJSON(data, &record) != nil {
		return nil
	}
	if record.Version != 1 || record.ID != id || record.Image != image || len(record.Files) != len(sorted) {
		return nil
	}
	for _, path := range sorted {
		if file, ok := record.Files[path]; !ok || !validImageFile(file) {
			return nil
		}
	}
	return record.Files
}

// RememberImageFiles records what a proof read out of one image. It is a memo:
// a write that fails costs the next launch a re-read, never a wrong answer.
func (s *Store) RememberImageFiles(image string, paths []string, files map[string]ImageFile) error {
	if err := s.intactAuthority(); err != nil {
		return err
	}
	id, sorted, err := s.imageFilesID(image, paths)
	if err != nil {
		return err
	}
	record := ImageFileRecord{Version: 1, ID: id, Image: image, Files: make(map[string]ImageFile, len(sorted))}
	for _, path := range sorted {
		file, ok := files[path]
		if !ok || !validImageFile(file) {
			return errors.New("an image file record needs one readable digest for every path it names")
		}
		record.Files[path] = file
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	// Replace rather than link: the same key describes the same bytes, so a
	// rewrite is idempotent and two racing writers agree by construction.
	return s.publish(imageFilesRecord(id), data, true)
}

func validImageFile(file ImageFile) bool {
	return lowerHex(file.SHA256, 64) && file.Size >= 0 && file.Mode >= 0
}
