// installtrivy installs only the reviewed release asset for this host.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

//go:embed checksums.txt
var checksums string

func main() {
	version := flag.String("version", "", "reviewed version from Make")
	dest := flag.String("dest", "", "installation path")
	flag.Parse()
	if err := install(*version, *dest); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func install(version, dest string) error {
	osName := map[string]string{"linux": "Linux", "darwin": "macOS"}[runtime.GOOS]
	arch := map[string]string{"amd64": "64bit", "arm64": "ARM64"}[runtime.GOARCH]
	if osName == "" || arch == "" || dest == "" {
		return errors.New("unsupported host or missing destination")
	}
	asset := "trivy_" + version + "_" + osName + "-" + arch + ".tar.gz"
	want := ""
	for line := range strings.Lines(checksums) {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			want = fields[0]
		}
	}
	if want == "" {
		return fmt.Errorf("no reviewed checksum for %s", asset)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	response, err := client.Get("https://github.com/aquasecurity/trivy/releases/download/v" + version + "/" + asset)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("trivy download: %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 150<<20))
	if err != nil {
		return err
	}
	bin, err := verifiedBinary(data, want)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".trivy-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(bin)
	err = errors.Join(writeErr, tmp.Chmod(0755), tmp.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return err
	}
	fmt.Println("Installed Trivy", version, "at", dest)
	return nil
}

func verifiedBinary(data []byte, want string) ([]byte, error) {
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != want {
		return nil, errors.New("trivy archive checksum mismatch")
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	for {
		h, err := archive.Next()
		if err == io.EOF {
			return nil, errors.New("trivy archive has no executable")
		}
		if err != nil {
			return nil, err
		}
		if h.Name != "trivy" {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size < 1 || h.Size > 300<<20 {
			return nil, errors.New("invalid Trivy executable")
		}
		return io.ReadAll(archive)
	}
}
