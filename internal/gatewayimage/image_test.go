package gatewayimage

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedGatewaySourcesExactlyMatchCheckout(t *testing.T) {
	root := filepath.Join("..", "..")
	expected := map[string]bool{"go.mod": true, "go.sum": true, "LICENSE": true}
	for _, dir := range []string{"cmd/coop-net", "internal/egress", "internal/networkgateway", "internal/networkview", "internal/safefile"} {
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
	// One dated snapshot replaces the image's apt sources: a second date, an undated URL or an append
	// would let a pin resolve against the live archive again.
	snapshot := regexp.MustCompile(`snapshot=https://snapshot\.ubuntu\.com/ubuntu/\d{8}T\d{6}Z \\\n`)
	if recipes != 1 || !strings.Contains(string(recipe), "-mod=readonly") || !strings.Contains(string(recipe), "nftables=1.0.2-1ubuntu3.1") ||
		len(snapshot.FindAllIndex(recipe, -1)) != 1 || strings.Count(string(recipe), "https://snapshot.ubuntu.com") != 1 ||
		!strings.Contains(string(recipe), `echo "deb $snapshot jammy main" > /etc/apt/sources.list`) {
		t.Fatal("missing pinned recipe contract")
	}
	for _, bad := range []string{".agent/secrets", "Dockerfile", "../go.mod", "internal/egress/../../private.go", "internal/egress/hello_test.go", "internal/egress/sub/other.go"} {
		if sourcePath(bad) {
			t.Fatalf("unsafe embedded path admitted: %s", bad)
		}
	}
}

// A network setup records the Go image coop-net is compiled with, so it must be the build stage's
// digest-pinned golang image: never a floating tag, and never the Envoy runtime stage.
func TestGoBaseIsTheRecipesPinnedBuildImage(t *testing.T) {
	base := GoBase()
	if !regexp.MustCompile(`^golang:[0-9a-z.-]+@sha256:[0-9a-f]{64}$`).MatchString(base) ||
		!strings.Contains(string(recipe), "FROM "+base+" AS build\n") {
		t.Fatalf("GoBase() = %q, want the digest-pinned golang image of the recipe's build stage", base)
	}
}
