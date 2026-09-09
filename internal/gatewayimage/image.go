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
	"io"
	"os"
	"path"
	"strings"

	"github.com/AndrewDryga/coop/internal/runtime"
)

//go:generate go -C ../.. run ./tools/gennetimage

//go:embed source.tar
var sources []byte

//go:embed Dockerfile
var recipe []byte

const BuildLabel = "coop.network.build"

func Fingerprint() string {
	hash := sha256.New()
	_, _ = hash.Write(recipe)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(sources)
	return hex.EncodeToString(hash.Sum(nil))
}

func Tag() string { return "coop-network:" + Fingerprint()[:32] }

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
	switch path.Dir(name) {
	case "cmd/coop-net", "internal/egress", "internal/networkgateway", "internal/networkview":
		return true
	}
	return false
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
