// Package fsimage packages release-owned filesystem enforcement without reading
// a user's checkout or requiring any compiler or BPF tools on their host.
package fsimage

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
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/runtime"
)

//go:generate go -C ../.. run ./tools/genfsimage
//go:embed source.tar
var sources []byte

//go:embed Dockerfile
var recipe []byte

const BuildLabel = "coop.filesystem.build"

func Fingerprint() string {
	hash := sha256.New()
	_, _ = hash.Write(recipe)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(sources)
	return hex.EncodeToString(hash.Sum(nil))
}

func Tag() string { return "coop-filesystem:" + Fingerprint()[:32] }

var directories = []string{"cmd/coop-fs", "internal/fsfilter", "internal/secretname"}
var fixedFiles = []string{"go.mod", "go.sum", "LICENSE", "internal/fsfilter/assets/filter.c", "internal/fsfilter/assets/loader.c", "internal/fsfilter/assets/shared.h"}

func sourcePath(name string) bool {
	return slices.Contains(fixedFiles, name) || name == path.Clean(name) && slices.Contains(directories, path.Dir(name)) && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

// Pack is only a developer/release operation. Operators build embedded sources.
func Pack(root string) ([]byte, error) {
	names := slices.Clone(fixedFiles)
	for _, dir := range directories {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := dir + "/" + entry.Name()
			if !entry.IsDir() && sourcePath(name) {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	var result bytes.Buffer
	w := tar.NewWriter(&result)
	for _, name := range names {
		file := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("invalid filesystem helper source %s", name)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if err := writeFile(w, name, data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}

func writeFile(w *tar.Writer, name string, data []byte) error {
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func Context() ([]byte, error) {
	var result bytes.Buffer
	w := tar.NewWriter(&result)
	if err := writeFile(w, "Dockerfile", recipe); err != nil {
		return nil, err
	}
	r := tar.NewReader(bytes.NewReader(sources))
	seen := map[string]bool{}
	for {
		header, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if !sourcePath(header.Name) || seen[header.Name] || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > 1<<20 || result.Len() > 4<<20 {
			return nil, errors.New("invalid embedded filesystem helper source")
		}
		seen[header.Name] = true
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		if err := writeFile(w, header.Name, data); err != nil {
			return nil, err
		}
	}
	for _, name := range fixedFiles {
		if !seen[name] {
			return nil, errors.New("incomplete embedded filesystem helper source")
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}

func Build(ctx context.Context, docker *runtime.Docker, log *os.File) (string, error) {
	data, err := Context()
	if err != nil {
		return "", err
	}
	arch := docker.Info().Architecture
	if arch == "aarch64" {
		arch = "arm64"
	}
	if arch == "x86_64" {
		arch = "amd64"
	}
	image, err := docker.BuildImage(ctx, runtime.DockerBuild{Tag: Tag(), Platform: "linux/" + arch, Labels: map[string]string{BuildLabel: Fingerprint()}}, data, log, log)
	if err != nil {
		return "", err
	}
	return image.ID, nil
}
