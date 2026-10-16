// Package gatewayimage builds release-owned networking helpers without a host
// compiler, repository Dockerfile, build hooks or workspace build context.
package gatewayimage

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/runtime"
)

//go:generate go -C ../.. run ./tools/gennetimage

//go:embed source.tar
var sources []byte

//go:embed Dockerfile
var recipe []byte

const BuildLabel = "coop.network.build"

func Fingerprint() string { return fingerprint(sources) }

func fingerprint(source []byte) string {
	hash := sha256.New()
	_, _ = hash.Write(recipe)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(source)
	return hex.EncodeToString(hash.Sum(nil))
}

// GoBase is the digest-pinned Go image the recipe's first stage compiles coop-net with, read from
// the recipe so the Dockerfile stays its one pin. A network setup records it beside the client
// image's Node base; the Fingerprint covers it too.
func GoBase() string {
	for line := range strings.Lines(string(recipe)) {
		if rest, ok := strings.CutPrefix(line, "FROM "); ok {
			image, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
			return image
		}
	}
	return ""
}

// sourceDirs hold the packages coop-net builds from: Pack takes their non-test Go
// files, and Context admits nothing else.
var sourceDirs = []string{"cmd/coop-net", "internal/egress", "internal/networkgateway", "internal/networkview", "internal/safefile"}

// sourceModules are the modules those packages load. Pack gives the image its own
// go.mod and go.sum naming only these, so bumping any other dependency of Coop
// leaves Fingerprint, and every user's network setup, unchanged.
var sourceModules = []string{"go.yaml.in/yaml/v3", "golang.org/x/net", "golang.org/x/sys"}

// Pack returns the source.tar for the checkout at root. Only the release-time
// generator (tools/gennetimage, via go generate) and the freshness test call it;
// Coop builds from the embedded copy and never reads a checkout.
func Pack(root string) ([]byte, error) {
	goMod, err := regularFile(root, "go.mod")
	if err != nil {
		return nil, err
	}
	goSum, err := regularFile(root, "go.sum")
	if err != nil {
		return nil, err
	}
	return pack(root, goMod, goSum)
}

func pack(root string, goMod, goSum []byte) ([]byte, error) {
	mod, sum, err := gatewayModule(goMod, goSum)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"go.mod": mod, "go.sum": sum}
	if files["LICENSE"], err = regularFile(root, "LICENSE"); err != nil {
		return nil, err
	}
	for _, dir := range sourceDirs {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := dir + "/" + entry.Name()
			if entry.IsDir() || !sourcePath(name) {
				continue
			}
			if files[name], err = regularFile(root, name); err != nil {
				return nil, err
			}
		}
	}
	var data bytes.Buffer
	archive := tar.NewWriter(&data)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(files[name])), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			return nil, err
		}
		if _, err := archive.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func regularFile(root, name string) ([]byte, error) {
	file := filepath.Join(root, filepath.FromSlash(name))
	if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("source is not a regular file: %s", name)
	}
	return os.ReadFile(file)
}

// gatewayModule narrows the checkout's go.mod and go.sum to sourceModules. The
// Dockerfile's Go image is the gateway's toolchain, so the toolchain line stays
// behind; any other directive, such as replace or godebug, stops the pack until
// someone decides whether coop-net needs it.
func gatewayModule(goMod, goSum []byte) ([]byte, []byte, error) {
	var module, goVersion string
	required := map[string]string{}
	inRequire := false
	for _, line := range strings.Split(string(goMod), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0 || strings.HasPrefix(fields[0], "//"):
		case inRequire && fields[0] == ")":
			inRequire = false
		case inRequire && len(fields) >= 2:
			required[fields[0]] = fields[1]
		case !inRequire && fields[0] == "require" && len(fields) == 2 && fields[1] == "(":
			inRequire = true
		case !inRequire && fields[0] == "require" && len(fields) >= 3:
			required[fields[1]] = fields[2]
		case !inRequire && fields[0] == "module" && len(fields) == 2:
			module = fields[1]
		case !inRequire && fields[0] == "go" && len(fields) == 2:
			goVersion = fields[1]
		case !inRequire && fields[0] == "toolchain":
		default:
			return nil, nil, fmt.Errorf("go.mod line %q has no place in the gateway's go.mod; decide whether coop-net needs it", line)
		}
	}
	mod := "module " + module + "\n\ngo " + goVersion + "\n\nrequire (\n"
	selected := map[string]bool{}
	for _, dependency := range sourceModules {
		version := required[dependency]
		if version == "" {
			return nil, nil, fmt.Errorf("go.mod does not require %s; drop it from sourceModules if coop-net no longer loads it", dependency)
		}
		mod += "\t" + dependency + " " + version + "\n"
		selected[dependency+" "+version] = true
	}
	mod += ")\n"
	var sum strings.Builder
	for _, line := range strings.SplitAfter(string(goSum), "\n") {
		if fields := strings.Fields(line); len(fields) == 3 && selected[fields[0]+" "+strings.TrimSuffix(fields[1], "/go.mod")] {
			sum.WriteString(line)
		}
	}
	return []byte(mod), []byte(sum.String()), nil
}

// Repository holds the network helper images, one tag per source fingerprint.
const Repository = "coop-network"

func Tag() string { return Repository + ":" + Fingerprint()[:32] }

// Context returns a small trusted tar stream. No filesystem is traversed during
// an operator build; source freshness is checked by the canonical developer gate.
func Context() ([]byte, error) {
	var result bytes.Buffer
	writer := tar.NewWriter(&result)
	if err := writer.WriteHeader(&tar.Header{Name: "Dockerfile", Size: int64(len(recipe)), Mode: 0644, Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := writer.Write(recipe); err != nil {
		return nil, err
	}
	reader := tar.NewReader(bytes.NewReader(sources))
	seen := map[string]bool{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil || !sourcePath(header.Name) || seen[header.Name] || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > 1<<20 || result.Len() > 4<<20 {
			return nil, errors.New("invalid embedded network helper source")
		}
		seen[header.Name] = true
		if err := writer.WriteHeader(&tar.Header{Name: header.Name, Size: header.Size, Mode: 0644, Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := io.Copy(writer, reader); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}

func sourcePath(name string) bool {
	if name == "go.mod" || name == "go.sum" || name == "LICENSE" {
		return true
	}
	if name != path.Clean(name) || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
		return false
	}
	return slices.Contains(sourceDirs, path.Dir(name))
}

// Build is an explicit operator build. Filtered admission does not silently
// install tooling, regenerate source or substitute an unqualified runtime.
func Build(ctx context.Context, docker *runtime.Docker, stdout, stderr *os.File) (string, error) {
	data, err := Context()
	if err != nil {
		return "", err
	}
	info := docker.Info()
	arch := info.Architecture
	if arch == "aarch64" {
		arch = "arm64"
	}
	if arch == "x86_64" {
		arch = "amd64"
	}
	image, err := docker.BuildImage(ctx, runtime.DockerBuild{Tag: Tag(), Platform: info.OSType + "/" + arch, Labels: map[string]string{BuildLabel: Fingerprint()}}, data, stdout, stderr)
	if err != nil {
		return "", err
	}
	return image.ID, nil
}
