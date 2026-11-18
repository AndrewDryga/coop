package forkspace

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// A BlobReader hands back each blob's exact bytes, one after another from one Git run, under both
// views. A tree, an object the store lacks, or a blob over the caller's limit is an error, and
// every read after a failure fails too: the stream is no longer in step.
func TestBlobReaderReadsExactBytesAndRefusesWhatItCannot(t *testing.T) {
	repo := committedSetupRepo(t)
	text := []byte("first line\nno final newline")
	binary := []byte{0, 1, 2, '\n', '\n'}
	commitFile(t, repo, "text.txt", text)
	commitFile(t, repo, "binary.bin", binary)
	object := func(spec string) string { return strings.TrimSpace(string(readOut(t, repo, "rev-parse", spec))) }
	textID, binaryID, tree := object("HEAD:text.txt"), object("HEAD:binary.bin"), object("HEAD^{tree}")
	missing := strings.Repeat("0", len(textID))

	for name, open := range map[string]func(context.Context, string) (*BlobReader, error){"view": GitBlobs, "observation": ObserveBlobs} {
		t.Run(name, func(t *testing.T) {
			reader, err := open(t.Context(), repo)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []struct {
				id      string
				content []byte
			}{{textID, text}, {binaryID, binary}, {textID, text}} {
				got, err := reader.Read(want.id, 1<<20)
				if err != nil || !bytes.Equal(got, want.content) {
					t.Fatalf("Read(%s) = %q, %v; want %q", want.id, got, err, want.content)
				}
			}
			if err := reader.Close(); err != nil {
				t.Fatalf("Close after good reads = %v", err)
			}

			for _, bad := range []struct {
				id    string
				limit int64
				want  string
			}{{tree, 1 << 20, "not a blob"}, {missing, 1 << 20, "missing"}, {textID, 3, "over the 3"}, {"HEAD", 1 << 20, "invalid object"}} {
				reader, err := open(t.Context(), repo)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := reader.Read(bad.id, bad.limit); err == nil || !strings.Contains(err.Error(), bad.want) {
					t.Fatalf("Read(%s, %d) = %v; want an error with %q", bad.id, bad.limit, err, bad.want)
				}
				if _, err := reader.Read(textID, 1<<20); err == nil {
					t.Fatal("a read after a failed one succeeded")
				}
				_ = reader.Close()
			}
		})
	}
}
