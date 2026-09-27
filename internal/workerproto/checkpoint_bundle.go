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
	return ReadWorkspaceCheckpointBundle(checkpoint, bytes.NewReader(bundle), nil)
}

// ReadWorkspaceCheckpointBundle validates a bounded manifest and streams each
// declared member. Consumers must quarantine any writes until this call succeeds.
func ReadWorkspaceCheckpointBundle(checkpoint WorkspaceCheckpoint, input io.Reader,
	visit func(*tar.Header, io.Reader) error,
) (WorkspaceCheckpointBundleManifest, error) {
	if err := checkpoint.Validate(); err != nil {
		return WorkspaceCheckpointBundleManifest{}, err
	}
	limited := &io.LimitedReader{R: input, N: checkpoint.Bundle.ByteSize + 1}
	digest := sha256.New()
	stream := io.TeeReader(limited, digest)
	reader := NewWorkspaceCheckpointTarReader(stream)
	header, err := reader.Next()
	if err != nil || header.Name != workspaceCheckpointManifestEntry {
		return WorkspaceCheckpointBundleManifest{}, errors.New("workspace checkpoint manifest must be the first tar member")
	}
	if err := validateCheckpointTarHeader(header, workspaceCheckpointManifestEntry, 0o644, header.Size, checkpoint.Version); err != nil {
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
	if manifest.Repository != nil {
		expected[0].entry = *manifest.Repository
	}
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
		if err := validateCheckpointTarHeader(header, member.entry.Entry, member.mode, member.entry.ByteSize, checkpoint.Version); err != nil {
			return WorkspaceCheckpointBundleManifest{}, err
		}
		memberHash := sha256.New()
		body := &io.LimitedReader{R: io.TeeReader(reader, memberHash), N: member.entry.ByteSize}
		if visit != nil {
			if err := visit(header, body); err != nil {
				return WorkspaceCheckpointBundleManifest{}, err
			}
		}
		_, err = io.Copy(io.Discard, body)
		if err != nil || body.N != 0 || hex.EncodeToString(memberHash.Sum(nil)) != member.entry.SHA256 {
			return WorkspaceCheckpointBundleManifest{}, fmt.Errorf("workspace checkpoint member %q identity does not match", member.entry.Entry)
		}
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		return WorkspaceCheckpointBundleManifest{}, errors.New("workspace checkpoint bundle contains undeclared members")
	}
	if _, err := io.Copy(checkpointZeroWriter{}, stream); err != nil || limited.N != 1 ||
		hex.EncodeToString(digest.Sum(nil)) != checkpoint.Bundle.SHA256 {
		return WorkspaceCheckpointBundleManifest{}, errors.New("workspace checkpoint bundle identity does not match")
	}
	return manifest, nil
}

type checkpointZeroWriter struct{}

func (checkpointZeroWriter) Write(data []byte) (int, error) {
	for _, value := range data {
		if value != 0 {
			return 0, errors.New("checkpoint has trailing data")
		}
	}
	return len(data), nil
}

// WorkspaceCheckpointTarReader accepts only physical regular-file headers and
// zero padding. archive/tar alone hides GNU/PAX extension records and padding,
// which would make the worker and controller validate different wire formats.
type WorkspaceCheckpointTarReader struct {
	input     io.Reader
	remaining int64
	padding   int64
	ended     bool
}

func NewWorkspaceCheckpointTarReader(input io.Reader) *WorkspaceCheckpointTarReader {
	return &WorkspaceCheckpointTarReader{input: input}
}

func (r *WorkspaceCheckpointTarReader) Read(data []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n, err := r.input.Read(data[:min(int64(len(data)), r.remaining)])
	r.remaining -= int64(n)
	if errors.Is(err, io.EOF) && r.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *WorkspaceCheckpointTarReader) Next() (*tar.Header, error) {
	if r.ended {
		return nil, io.EOF
	}
	if r.remaining != 0 {
		return nil, errors.New("checkpoint member was not fully consumed")
	}
	if _, err := io.CopyN(checkpointZeroWriter{}, r.input, r.padding); err != nil {
		return nil, err
	}
	var raw [512]byte
	if _, err := io.ReadFull(r.input, raw[:]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if raw == [512]byte{} {
		if _, err := io.ReadFull(r.input, raw[:]); err != nil || raw != [512]byte{} {
			return nil, errors.New("checkpoint tar terminator is invalid")
		}
		n, err := io.Copy(checkpointZeroWriter{}, r.input)
		if err != nil || n%512 != 0 {
			return nil, errors.New("checkpoint tar trailer is invalid")
		}
		r.ended = true
		return nil, io.EOF
	}
	if raw[156] != tar.TypeReg {
		return nil, errors.New("checkpoint tar extensions and special files are not allowed")
	}
	for _, field := range [][]byte{raw[:100], raw[157:257], raw[265:297], raw[297:329]} {
		if nul := bytes.IndexByte(field, 0); nul >= 0 && !bytes.Equal(field[nul:], make([]byte, len(field)-nul)) {
			return nil, errors.New("checkpoint header contains hidden text")
		}
	}
	if !bytes.Equal(raw[345:], make([]byte, 167)) {
		return nil, errors.New("checkpoint header contains unsupported extension data")
	}
	header, err := tar.NewReader(bytes.NewReader(raw[:])).Next()
	if err != nil {
		return nil, err
	}
	r.remaining, r.padding = header.Size, (512-header.Size%512)%512
	return header, nil
}

func validateCheckpointTarHeader(header *tar.Header, name string, mode, size int64, version int) error {
	format := tar.FormatGNU
	if version == 1 {
		format = tar.FormatUSTAR
	}
	epoch := time.Unix(0, 0).UTC()
	if header == nil || header.Name != name || header.Typeflag != tar.TypeReg || header.Mode != mode ||
		header.Size != size || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" ||
		header.Linkname != "" || !header.ModTime.Equal(epoch) || !header.AccessTime.IsZero() ||
		!header.ChangeTime.IsZero() || header.Devmajor != 0 || header.Devminor != 0 ||
		len(header.PAXRecords) != 0 || header.Format != format {
		return fmt.Errorf("workspace checkpoint tar header %q is invalid", name)
	}
	return nil
}
