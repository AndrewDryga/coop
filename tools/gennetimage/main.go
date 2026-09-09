// gennetimage packages only trusted gateway source for release-time embedding.
// It is a developer command, never invoked by coop build or a user repository.
package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate() error {
	files := []string{"go.mod", "go.sum", "LICENSE"}
	for _, dir := range []string{"cmd/coop-net", "internal/egress", "internal/networkgateway", "internal/networkview"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
				files = append(files, filepath.ToSlash(filepath.Join(dir, entry.Name())))
			}
		}
	}
	slices.Sort(files)
	var data bytes.Buffer
	archive := tar.NewWriter(&data)
	for _, path := range files {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("source is not a regular file: %s", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := archive.WriteHeader(&tar.Header{Name: path, Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			return err
		}
		if _, err := archive.Write(content); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll("internal/gatewayimage", 0755); err != nil {
		return err
	}
	if err := os.WriteFile("internal/gatewayimage/source.tar", data.Bytes(), 0644); err != nil {
		return err
	}
	fmt.Printf("packaged %d trusted source files (%d bytes)\n", len(files), data.Len())
	return nil
}
