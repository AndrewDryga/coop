package workerproto

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// ValidateWorkspaceCheckpointBundle authenticates every tar member against the external
// descriptor and inner manifest before a caller uploads or restores any bytes.
func ValidateWorkspaceCheckpointBundle(
	checkpoint WorkspaceCheckpoint,
	bundle []byte,
) (WorkspaceCheckpointBundleManifest, error) {
	if err := checkpoint.Validate(); err != nil {
		return WorkspaceCheckpointBundleManifest{}, err
	}
	if len(bundle) == 0 || len(bundle) > MaxWorkspaceCheckpointBundleBytes ||
		int64(len(bundle)) != checkpoint.Bundle.ByteSize || checkpointBundleSHA256(bundle) != checkpoint.Bundle.SHA256 {
		return WorkspaceCheckpointBundleManifest{}, errors.New("workspace checkpoint bundle identity does not match")
	}
	reader := tar.NewReader(bytes.NewReader(bundle))
	header, err := reader.Next()
	if err != nil || header.Name != workspaceCheckpointManifestEntry {
		return WorkspaceCheckpointBundleManifest{}, errors.New("workspace checkpoint manifest must be the first tar member")
	}
	if err := validateCheckpointTarHeader(header, workspaceCheckpointManifestEntry, 0o644, header.Size); err != nil {
		return WorkspaceCheckpointBundleManifest{}, err
	}
	if header.Size <= 0 || header.Size > MaxWorkspaceCheckpointManifest {
		return WorkspaceCheckpointBundleManifest{}, errors.New("workspace checkpoint manifest exceeds its bound")
	}
	manifestBytes, err := io.ReadAll(io.LimitReader(reader, MaxWorkspaceCheckpointManifest+1))
	if err != nil || int64(len(manifestBytes)) != header.Size {
		return WorkspaceCheckpointBundleManifest{}, errors.New("workspace checkpoint manifest length does not match")
	}
	manifest, err := DecodeWorkspaceCheckpointBundleManifest(manifestBytes)
	if err != nil {
		return WorkspaceCheckpointBundleManifest{}, err
	}
	if err := ValidateWorkspaceCheckpointPair(checkpoint, manifest); err != nil {
		return WorkspaceCheckpointBundleManifest{}, err
	}
	type expectedMember struct {
		entry WorkspaceCheckpointBundleEntry
		mode  int64
	}
	expected := []expectedMember{{entry: manifest.TrackedPatch, mode: 0o644}}
	for _, file := range manifest.UntrackedFiles {
		expected = append(expected, expectedMember{
			entry: WorkspaceCheckpointBundleEntry{Entry: file.Entry, SHA256: file.SHA256, ByteSize: file.ByteSize},
			mode:  file.Mode,
		})
	}
	for _, file := range manifest.TaskProjection.Files {
		expected = append(expected, expectedMember{
			entry: WorkspaceCheckpointBundleEntry{Entry: file.Entry, SHA256: file.SHA256, ByteSize: file.ByteSize},
			mode:  file.Mode,
		})
	}
	if manifest.GateReceipt != nil {
		expected = append(expected, expectedMember{entry: *manifest.GateReceipt, mode: 0o644})
	}
	for _, member := range expected {
		header, err := reader.Next()
		if err != nil {
			return WorkspaceCheckpointBundleManifest{}, fmt.Errorf("workspace checkpoint member %q is missing", member.entry.Entry)
		}
		if err := validateCheckpointTarHeader(header, member.entry.Entry, member.mode, member.entry.ByteSize); err != nil {
			return WorkspaceCheckpointBundleManifest{}, err
		}
		digest := sha256.New()
		written, err := io.Copy(digest, io.LimitReader(reader, member.entry.ByteSize+1))
		if err != nil || written != member.entry.ByteSize || hex.EncodeToString(digest.Sum(nil)) != member.entry.SHA256 {
			return WorkspaceCheckpointBundleManifest{}, fmt.Errorf("workspace checkpoint member %q identity does not match", member.entry.Entry)
		}
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		return WorkspaceCheckpointBundleManifest{}, errors.New("workspace checkpoint bundle contains undeclared members")
	}
	return manifest, nil
}

func validateCheckpointTarHeader(header *tar.Header, name string, mode, size int64) error {
	epoch := time.Unix(0, 0).UTC()
	if header == nil || header.Name != name || header.Typeflag != tar.TypeReg || header.Mode != mode ||
		header.Size != size || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" ||
		header.Linkname != "" || !header.ModTime.Equal(epoch) || !header.AccessTime.IsZero() ||
		!header.ChangeTime.IsZero() || header.Devmajor != 0 || header.Devminor != 0 ||
		len(header.PAXRecords) != 0 || header.Format != tar.FormatUSTAR {
		return fmt.Errorf("workspace checkpoint tar header %q is invalid", name)
	}
	return nil
}

func checkpointBundleSHA256(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
