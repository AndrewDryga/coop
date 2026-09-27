package workerproto

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
)

func TestCapturedCheckpointV2Golden(t *testing.T) {
	data, err := os.ReadFile("../../testdata/protocol/workspace-checkpoint-v2.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Checkpoint WorkspaceCheckpoint `json:"checkpoint"`
		Bundle     string              `json:"bundle_base64"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	bundle, err := base64.StdEncoding.DecodeString(fixture.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ReadWorkspaceCheckpointBundle(fixture.Checkpoint, bytes.NewReader(bundle), nil)
	if err != nil || manifest.Version != 2 || manifest.Repository == nil || len(manifest.UntrackedFiles) != 1 {
		t.Fatalf("captured checkpoint interoperability: %+v, %v", manifest, err)
	}
}

func TestCheckpointArchiveRequiresPhysicalHeadersPaddingAndTerminator(t *testing.T) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: "body", Typeflag: tar.TypeReg, Mode: 0644, Size: 1, Format: tar.FormatGNU}); err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("x"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	valid := buffer.Bytes()
	for _, variant := range []string{"valid", "extension", "padding", "missing trailer", "short trailer", "unaligned trailer", "hidden header"} {
		t.Run(variant, func(t *testing.T) {
			data := bytes.Clone(valid)
			switch variant {
			case "extension":
				data[156] = tar.TypeGNULongName
			case "padding":
				data[513] = 1
			case "missing trailer":
				data = data[:1024]
			case "short trailer":
				data = data[:1536]
			case "unaligned trailer":
				data = append(data, 0)
			case "hidden header":
				data[99] = 'x'
			}
			reader := NewWorkspaceCheckpointTarReader(bytes.NewReader(data))
			_, err := reader.Next()
			if err == nil {
				_, err = io.Copy(io.Discard, reader)
			}
			if err == nil {
				_, err = reader.Next()
			}
			if variant == "valid" && !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			if variant != "valid" && (err == nil || errors.Is(err, io.EOF)) {
				t.Fatal("accepted ambiguous checkpoint tar")
			}
		})
	}
}

func TestCheckpointArchiveReadsGNUSizeWithoutAllocatingLargeMember(t *testing.T) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	const size = 1 << 33
	if err := writer.WriteHeader(&tar.Header{Name: "body", Typeflag: tar.TypeReg, Mode: 0644, Size: size, Format: tar.FormatGNU}); err != nil {
		t.Fatal(err)
	}
	reader := NewWorkspaceCheckpointTarReader(&buffer)
	header, err := reader.Next()
	if err != nil || header.Size != size {
		t.Fatalf("GNU large member: %+v, %v", header, err)
	}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated large member: %v", err)
	}
}
