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
	"testing/iotest"
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

// The upstream revision is optional: a bundle without it keeps the v15.0.0 shape.
func TestCheckpointManifestUpstreamRevisionIsOptionalAndExact(t *testing.T) {
	data, err := os.ReadFile("../../testdata/protocol/workspace-checkpoint-v2.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Bundle string `json:"bundle_base64"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	bundle, err := base64.StdEncoding.DecodeString(fixture.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(bundle))
	var document []byte
	for document == nil {
		header, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == workspaceCheckpointManifestEntry {
			if document, err = io.ReadAll(reader); err != nil {
				t.Fatal(err)
			}
		}
	}
	var original map[string]any
	if err := json.Unmarshal(document, &original); err != nil {
		t.Fatal(err)
	}
	if _, ok := original["upstream_revision"]; ok {
		t.Fatal("the v2 golden must stay the shape without an upstream")
	}
	base, _ := original["base_revision"].(string)
	other := "1111111111111111111111111111111111111111"
	for name, tc := range map[string]struct {
		value any
		valid bool
	}{
		"newer commit": {other, true},
		"the base":     {base, false},
		"short id":     {other[:12], false},
		"not a string": {float64(1), false},
	} {
		t.Run(name, func(t *testing.T) {
			value := map[string]any{}
			for key, field := range original {
				value[key] = field
			}
			value["upstream_revision"] = tc.value
			changed, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := DecodeWorkspaceCheckpointBundleManifest(changed)
			if tc.valid != (err == nil) || (tc.valid && manifest.UpstreamRevision != other) {
				t.Fatalf("upstream %v: %q, %v", tc.value, manifest.UpstreamRevision, err)
			}
		})
	}
	legacy, err := os.ReadFile("../../testdata/protocol/workspace-checkpoint-bundle-v1.manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var historical map[string]any
	if err := json.Unmarshal(legacy, &historical); err != nil {
		t.Fatal(err)
	}
	historical["upstream_revision"] = other
	changed, err := json.Marshal(historical)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeWorkspaceCheckpointBundleManifest(changed); err == nil {
		t.Fatal("a historical v1 manifest accepted an upstream revision")
	}
}

// Only the bundle's own bytes may be blamed on the bundle: a broken input or a failing consumer
// is not a bundle a controller must stop sending.
func TestCheckpointBundleFaultsAreToldApartFromReadAndConsumerFaults(t *testing.T) {
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
	corrupt := bytes.Clone(bundle)
	corrupt[len(corrupt)/2] ^= 0xff
	if _, err := ReadWorkspaceCheckpointBundle(fixture.Checkpoint, bytes.NewReader(corrupt), nil); !errors.Is(err, ErrInvalidWorkspaceCheckpointBundle) {
		t.Fatalf("corrupt bundle = %v, want an invalid bundle", err)
	}
	broken := errors.New("connection reset")
	input := io.MultiReader(bytes.NewReader(bundle[:1024]), iotest.ErrReader(broken))
	if _, err := ReadWorkspaceCheckpointBundle(fixture.Checkpoint, input, nil); errors.Is(err, ErrInvalidWorkspaceCheckpointBundle) {
		t.Fatalf("broken input = %v, blamed on the bundle", err)
	}
	full := errors.New("disk full")
	visit := func(*tar.Header, io.Reader) error { return full }
	if _, err := ReadWorkspaceCheckpointBundle(fixture.Checkpoint, bytes.NewReader(bundle), visit); !errors.Is(err, full) || errors.Is(err, ErrInvalidWorkspaceCheckpointBundle) {
		t.Fatalf("failing consumer = %v, want its own error", err)
	}
}
