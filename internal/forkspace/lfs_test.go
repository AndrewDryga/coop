package forkspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

type lfsStagingEditReader struct {
	reader io.Reader
	edit   func()
}

func (r *lfsStagingEditReader) Read(p []byte) (int, error) {
	if r.edit != nil {
		r.edit()
		r.edit = nil
	}
	return r.reader.Read(p)
}

func TestLFSPublicationPreservesEditsDuringPayloadStaging(t *testing.T) {
	repository, commit, _ := lfsFixture(t)
	root, err := os.OpenRoot(repository)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var pointer LFSPointer
	if err := VisitLFSPointers(t.Context(), repository, commit, func(p LFSPointer) error {
		if p.Path == "nested/large.bin" {
			pointer = p
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	info, err := root.Lstat(pointer.Path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := root.Open(lfsObjectPath(pointer.OID))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ownerBytes := []byte("intervening owner work\n")
	reader := &lfsStagingEditReader{reader: file, edit: func() {
		if err := os.WriteFile(filepath.Join(repository, pointer.Path), ownerBytes, pointer.Mode); err != nil {
			t.Fatal(err)
		}
	}}
	if err := writeLFSFile(t.Context(), root, pointer.Path, pointer.Mode, reader, pointer,
		func() error { return requireLFSPointer(root, pointer, info) }); err == nil {
		t.Fatal("staged payload overwrote intervening owner work")
	}
	got, err := os.ReadFile(filepath.Join(repository, pointer.Path))
	if err != nil || !bytes.Equal(got, ownerBytes) {
		t.Fatal("owner bytes lost", err)
	}
	entries, err := os.ReadDir(filepath.Join(repository, "nested"))
	if err != nil || len(entries) != 1 {
		t.Fatal("failed staging did not clean its temporary payload", err)
	}
}

func lfsFixture(t *testing.T) (string, string, map[string][]byte) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repository, git := gitrepo.New(t)
	files := map[string][]byte{"nested/large.bin": bytes.Repeat([]byte("large\x00content\n"), 400_000), "empty.bin": {}}
	if err := os.MkdirAll(filepath.Join(repository, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for path, data := range files {
		oid := fmt.Sprintf("%x", sha256.Sum256(data))
		pointer := []byte(fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(data)))
		if err := os.WriteFile(filepath.Join(repository, path), pointer, 0755); err != nil {
			t.Fatal(err)
		}
		object := filepath.Join(repository, lfsObjectPath(oid))
		if err := os.MkdirAll(filepath.Dir(object), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(object, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repository, "pointer-example.txt"), pointer, 0644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "LFS source")
	commit, err := gitOutputContext(context.Background(), repository, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return repository, commit, files
}

func TestLFSCopiesCompleteObjectsOfflineIncludingEmptyFiles(t *testing.T) {
	source, commit, files := lfsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := GitClonePinnedContext(ctx, source, workspace, commit); err != nil {
		t.Fatal(err)
	}
	if err := GitDetach(ctx, workspace, commit); err != nil {
		t.Fatal(err)
	}
	documentation, err := os.ReadFile(filepath.Join(workspace, "pointer-example.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := HydrateLFS(ctx, workspace, commit); err == nil {
		t.Fatal("hydrated without required objects")
	}
	for range 2 {
		if err := HydrateLFS(ctx, workspace, commit, source); err != nil {
			t.Fatal(err)
		}
		if err := VerifyLFS(ctx, workspace, commit); err != nil {
			t.Fatal(err)
		}
	}
	for path, expected := range files {
		actual, err := os.ReadFile(filepath.Join(workspace, path))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("incomplete %q: %d bytes, %v", path, len(actual), err)
		}
		oid := fmt.Sprintf("%x", sha256.Sum256(expected))
		left, _ := os.Stat(filepath.Join(source, lfsObjectPath(oid)))
		right, err := os.Stat(filepath.Join(workspace, lfsObjectPath(oid)))
		if err != nil || os.SameFile(left, right) {
			t.Fatalf("LFS object is not independently owned: %v", err)
		}
		info, err := os.Stat(filepath.Join(workspace, path))
		if err != nil || info.Mode().Perm()&0111 == 0 {
			t.Fatalf("lost executable file mode: %v", err)
		}
	}
	if actual, err := os.ReadFile(filepath.Join(workspace, "pointer-example.txt")); err != nil || !bytes.Equal(actual, documentation) {
		t.Fatal("hydrated an ordinary pointer example without pinned LFS attributes")
	}
	if err := os.WriteFile(filepath.Join(workspace, "empty.bin"), []byte("new model work"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := HydrateLFS(ctx, workspace, commit, source); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("overwrote changed LFS file: %v", err)
	}
	if err := VerifyLFS(ctx, workspace, commit); err == nil {
		t.Fatal("verified modified content")
	}
}

func TestLFSRefusesCorruptionRedirectsAndUnpinnedAttributes(t *testing.T) {
	for _, scenario := range []string{"corrupt", "outside-object", "changed-index", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			source, commit, files := lfsFixture(t)
			workspace := filepath.Join(t.TempDir(), "workspace")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := GitClonePinnedContext(ctx, source, workspace, commit); err != nil {
				t.Fatal(err)
			}
			if err := GitDetach(ctx, workspace, commit); err != nil {
				t.Fatal(err)
			}
			object := filepath.Join(source, lfsObjectPath(fmt.Sprintf("%x", sha256.Sum256(files["nested/large.bin"]))))
			switch scenario {
			case "corrupt":
				if err := os.WriteFile(object, bytes.Repeat([]byte("x"), len(files["nested/large.bin"])), 0600); err != nil {
					t.Fatal(err)
				}
			case "outside-object":
				outside := filepath.Join(t.TempDir(), "payload")
				if err := os.Rename(object, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, object); err != nil {
					t.Fatal(err)
				}
			case "changed-index":
				gitIn(t, workspace, "update-index", "--force-remove", ".gitattributes")
			case "canceled":
				cancel()
			}
			if err := HydrateLFS(ctx, workspace, commit, source); err == nil {
				t.Fatalf("accepted %s", scenario)
			}
			if data, err := os.ReadFile(filepath.Join(workspace, "nested/large.bin")); err != nil || !bytes.HasPrefix(data, []byte("version https://git-lfs.github.com")) {
				t.Fatal("published incomplete LFS data")
			}
		})
	}
}

func TestLFSStreamingHashObservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyLFS(ctx, &bytes.Buffer{}, strings.NewReader(""), 0, fmt.Sprintf("%x", sha256.Sum256(nil))); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled copy: %v", err)
	}
}

func TestLFSRefusesUnsupportedPointersInsteadOfLeavingThemAsPayload(t *testing.T) {
	for _, variant := range []string{"crlf", "whitespace", "short", "extended", "https://hawser.github.com/spec/v1", "http://git-media.io/v/2"} {
		t.Run(variant, func(t *testing.T) {
			source, _, _ := lfsFixture(t)
			path := filepath.Join(source, "empty.bin")
			pointer, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "crlf":
				pointer = bytes.ReplaceAll(pointer, []byte("\n"), []byte("\r\n"))
			case "whitespace":
				pointer = append([]byte("\n"), pointer...)
			case "short":
				pointer = []byte("version https://git-lfs.github.com/spec/v1\n")
			case "extended":
				pointer = append(pointer, bytes.Repeat([]byte("extension unsupported\n"), 100)...)
			default:
				pointer = bytes.ReplaceAll(pointer, []byte("https://git-lfs.github.com/spec/v1"), []byte(variant))
			}
			if err := os.WriteFile(path, pointer, 0755); err != nil {
				t.Fatal(err)
			}
			gitIn(t, source, "add", "empty.bin")
			gitIn(t, source, "commit", "-qm", "noncanonical pointer")
			commit, err := gitOutputContext(context.Background(), source, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if err := HydrateLFS(context.Background(), source, commit); err == nil {
				t.Fatal("silently left an unsupported LFS pointer in the working tree")
			}
		})
	}
}
