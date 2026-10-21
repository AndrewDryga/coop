package gatewayimage

import (
	"archive/tar"
	"bytes"
	"go/version"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestEmbeddedGatewaySourcesExactlyMatchCheckout(t *testing.T) {
	packed, err := Pack(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sources, packed) {
		return
	}
	embedded, fresh := tarFiles(t, sources), tarFiles(t, packed)
	var drift []string
	for name, data := range fresh {
		if old, ok := embedded[name]; !ok || !bytes.Equal(old, data) {
			drift = append(drift, name)
		}
	}
	for name := range embedded {
		if _, ok := fresh[name]; !ok {
			drift = append(drift, name)
		}
	}
	slices.Sort(drift)
	t.Fatalf("embedded gateway sources drifted from the checkout %v; run go generate ./internal/gatewayimage", drift)
}

// The image builds coop-net with -mod=readonly from the packed go.mod and go.sum
// alone, so they must require every module its Linux build loads and carry every
// checksum that load verifies.
func TestEmbeddedGatewayModuleLoadsOnItsOwn(t *testing.T) {
	dir := t.TempDir()
	for name, data := range tarFiles(t, sources) {
		file := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list := exec.Command("go", "list", "-deps", "./cmd/coop-net")
	list.Dir = dir
	list.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOWORK=off")
	if out, err := list.CombinedOutput(); err != nil {
		t.Fatalf("the embedded gateway module does not load on its own; add any internal package coop-net now imports to sourceDirs and any module it loads to sourceModules, then run go generate ./internal/gatewayimage: %v\n%s", err, out)
	}
}

// The recipe must build the packed module and carry the repository's Go security patches.
// Pack drops the toolchain directive, so checking only its go line misses patch updates.
func TestGatewayRecipeBuildsThePackedGoVersion(t *testing.T) {
	recipe, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	image := regexp.MustCompile(`(?m)^FROM golang:([0-9.]+)-`).FindSubmatch(recipe)
	module := regexp.MustCompile(`(?m)^go ([0-9.]+)$`).FindSubmatch(tarFiles(t, sources)["go.mod"])
	if image == nil || module == nil {
		t.Fatalf("cannot read the recipe's golang image or the packed go line")
	}
	if version.Compare("go"+string(image[1]), "go"+string(module[1])) < 0 {
		t.Fatalf("the recipe builds with Go %s, older than the packed go %s line", image[1], module[1])
	}
	rootModule, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	toolchain := regexp.MustCompile(`(?m)^toolchain (go[0-9.]+)$`).FindSubmatch(rootModule)
	if toolchain == nil {
		t.Fatal("cannot read the repository's pinned Go toolchain")
	}
	if version.Compare("go"+string(image[1]), string(toolchain[1])) < 0 {
		t.Fatalf("the recipe builds with Go %s, older than the repository toolchain %s", image[1], toolchain[1])
	}
}

// Every Dependabot bump used to change the embedded go.mod and go.sum, and with
// them every filtered user's network setup. Only a bump of a module coop-net loads
// may move the fingerprint now.
func TestOnlyGatewayModuleBumpsMoveTheFingerprint(t *testing.T) {
	root := filepath.Join("..", "..")
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	goSum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	unrelated := map[string]bool{}
	for _, line := range strings.Split(string(goSum), "\n") {
		if fields := strings.Fields(line); len(fields) == 3 && !slices.Contains(sourceModules, fields[0]) {
			unrelated[fields[0]] = true
		}
	}
	fingerprintAfter := func(bumped map[string]bool) string {
		t.Helper()
		packed, err := pack(root, bump(goMod, bumped), bump(goSum, bumped))
		if err != nil {
			t.Fatal(err)
		}
		return fingerprint(packed)
	}
	before := fingerprintAfter(nil)
	if fingerprintAfter(unrelated) != before {
		t.Errorf("bumping %d modules coop-net never loads moved the gateway fingerprint", len(unrelated))
	}
	if fingerprintAfter(map[string]bool{"golang.org/x/net": true}) == before {
		t.Error("bumping golang.org/x/net left the gateway fingerprint unchanged")
	}
}

// bump moves each listed module to a new version in go.mod or go.sum text, the way
// a dependency update does.
func bump(data []byte, modules map[string]bool) []byte {
	lines := strings.SplitAfter(string(data), "\n")
	for i, line := range lines {
		if fields := strings.Fields(line); len(fields) >= 2 && modules[fields[0]] {
			lines[i] = strings.Replace(line, " v", " v9", 1)
		}
	}
	return []byte(strings.Join(lines, ""))
}

func TestGatewayModuleRefusesWhatItCannotCarry(t *testing.T) {
	goMod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{
		string(goMod) + "\nreplace golang.org/x/net => ../net\n",
		string(goMod) + "\ngodebug default=go1.21\n",
		strings.Replace(string(goMod), "\tgolang.org/x/sys ", "\tgolang.org/x/sysx ", 1),
	} {
		if _, _, err := gatewayModule([]byte(changed), nil); err == nil {
			t.Errorf("packed a gateway module from:\n%s", changed)
		}
	}
}

func tarFiles(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if files[header.Name], err = io.ReadAll(reader); err != nil {
			t.Fatal(err)
		}
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
