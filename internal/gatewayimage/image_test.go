package gatewayimage

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedGatewaySourcesExactlyMatchCheckout(t *testing.T) {
	root := filepath.Join("..", "..")
	expected := map[string]bool{"go.mod": true, "go.sum": true, "LICENSE": true}
	for _, dir := range []string{"cmd/coop-net", "internal/egress", "internal/networkgateway", "internal/networkview"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
				expected[dir+"/"+entry.Name()] = true
			}
		}
	}
	archive := tar.NewReader(bytes.NewReader(sources))
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !expected[header.Name] || header.Typeflag != tar.TypeReg || header.Linkname != "" || !sourcePath(header.Name) {
			t.Fatalf("unexpected/duplicate embedded source: %s", header.Name)
		}
		data, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(filepath.Join(root, header.Name))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("source is not a regular file: %s", header.Name)
		}
		actual, err := os.ReadFile(filepath.Join(root, header.Name))
		if err != nil || !bytes.Equal(data, actual) {
			t.Fatalf("embedded gateway source drift: %s; run go run ./tools/gennetimage from repo root", header.Name)
		}
		delete(expected, header.Name)
	}
	if len(expected) != 0 {
		t.Fatalf("missing embedded sources: %v; run go run ./tools/gennetimage", expected)
	}
}

func TestGatewayBuildContextContainsOnlyTrustedSourceAndRecipe(t *testing.T) {
	data, err := Context()
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(data))
	recipes := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "Dockerfile" {
			recipes++
		} else if !sourcePath(header.Name) {
			t.Fatalf("build context contains non-source path %s", header.Name)
		}
	}
	if recipes != 1 || !strings.Contains(string(recipe), "-mod=readonly") || !strings.Contains(string(recipe), "nftables=1.0.2-1ubuntu3.1") {
		t.Fatal("missing pinned recipe contract")
	}
	for _, bad := range []string{".agent/secrets", "Dockerfile", "../go.mod", "internal/egress/../../private.go", "internal/egress/hello_test.go", "internal/egress/sub/other.go"} {
		if sourcePath(bad) {
			t.Fatalf("unsafe embedded path admitted: %s", bad)
		}
	}
}
