// installshellcheck installs only the reviewed ShellCheck release asset for this host, the way
// tools/installtrivy installs the image scanner.
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
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if (runtime.GOOS != "linux" && runtime.GOOS != "darwin") || arch == "" || version == "" || dest == "" {
		return errors.New("unsupported host, or missing version or destination")
	}
	asset := "shellcheck-v" + version + "." + runtime.GOOS + "." + arch + ".tar.gz"
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
	response, err := client.Get("https://github.com/koalaman/shellcheck/releases/download/v" + version + "/" + asset)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("shellcheck download: %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 100<<20))
	if err != nil {
		return err
	}
	bin, err := verifiedBinary(data, want, "shellcheck-v"+version+"/shellcheck")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".shellcheck-")
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
	fmt.Println("Installed ShellCheck", version, "at", dest)
	return nil
}

// verifiedBinary checks the archive's digest before reading it, then returns the one regular file
// at name: a link or any other entry is never installed.
func verifiedBinary(data []byte, want, name string) ([]byte, error) {
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != want {
		return nil, errors.New("shellcheck archive checksum mismatch")
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
			return nil, errors.New("shellcheck archive has no executable")
		}
		if err != nil {
			return nil, err
		}
		if h.Name != name {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size < 1 || h.Size > 200<<20 {
			return nil, errors.New("invalid ShellCheck executable")
		}
		return io.ReadAll(archive)
	}
}
