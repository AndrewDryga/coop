package fsimage

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedFilesystemSourcesExactlyMatchCheckout(t *testing.T) {
	packed, err := Pack(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packed, sources) {
		t.Fatal("embedded filesystem sources drifted; run go generate ./internal/fsimage")
	}
}

func TestEmbeddedFilesystemModuleBuildsWithoutCheckout(t *testing.T) {
	dir := t.TempDir()
	r := tar.NewReader(bytes.NewReader(sources))
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-mod=readonly", "-o", filepath.Join(dir, "coop-fs"), "./cmd/coop-fs")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("embedded helper cannot build alone: %v\n%s", err, out)
	}
}

func TestFilesystemContextContainsOnlyReleaseSources(t *testing.T) {
	data, err := Context()
	if err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(bytes.NewReader(data))
	seen := map[string]bool{}
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg || seen[h.Name] || h.Name != "Dockerfile" && !sourcePath(h.Name) {
			t.Fatalf("unexpected build input %#v", h)
		}
		seen[h.Name] = true
	}
	for _, name := range []string{"Dockerfile", "cmd/coop-fs/main_linux.go", "cmd/coop-fs/admit_linux.go", "internal/fsfilter/assets/filter.c", "internal/fsfilter/assets/loader.c", "go.mod", "go.sum"} {
		if !seen[name] {
			t.Fatal("missing source", name)
		}
	}
	if !strings.HasPrefix(Tag(), "coop-filesystem:") || len(Fingerprint()) != 64 {
		t.Fatal("invalid image identity")
	}
}

func TestFilesystemContextRefusesMalformedEmbeddedSources(t *testing.T) {
	original := sources
	defer func() { sources = original }()
	for _, h := range []*tar.Header{{Name: "../outside", Typeflag: tar.TypeReg}, {Name: "go.mod", Typeflag: tar.TypeSymlink, Linkname: "/outside"}, {Name: "internal/fsfilter/assets/filter.o", Typeflag: tar.TypeReg}} {
		var out bytes.Buffer
		w := tar.NewWriter(&out)
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		sources = out.Bytes()
		if _, err := Context(); err == nil {
			t.Fatal("accepted untrusted embedded input", h.Name)
		}
	}
	sources = nil
	if _, err := Context(); err == nil {
		t.Fatal("accepted missing sources")
	}
}
