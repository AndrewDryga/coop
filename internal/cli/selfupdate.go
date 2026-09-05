package cli

// Self-update replaces the running coop binary with a newer GitHub release. It
// downloads the versioned GoReleaser archive and checksum as data, verifies them
// locally, and never executes a downloaded installer.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// These are vars (not consts) so tests can point them at an httptest server.
var (
	githubLatestURL = "https://api.github.com/repos/AndrewDryga/coop/releases/latest"

	releaseFileURLFor = func(tag, name string) string {
		return "https://github.com/AndrewDryga/coop/releases/download/" + tag + "/" + name
	}

	// executablePath resolves the running binary; a var so tests can stub it.
	executablePath = os.Executable

	updateHTTPClient = &http.Client{Timeout: 30 * time.Second}

	// Release assets are megabytes over whatever link the host has, so they get a whole-transfer
	// bound of minutes rather than the API client's 30s — which timed out real updates on slow
	// connections — plus the explicit size caps below.
	releaseHTTPClient = &http.Client{Timeout: 10 * time.Minute}
)

// Release files are fully verified before use; the caps only keep a wrong or hostile response from
// exhausting memory before the checksum ever runs. A stripped coop binary is a few MB.
const (
	maxReleaseChecksumsBytes = 1 << 20
	maxReleaseArchiveBytes   = 256 << 20
	maxReleaseBinaryBytes    = 256 << 20
)

// checkError marks a soft failure: coop couldn't determine the latest release
// (offline, GitHub rate limit). In a combined `coop update` this is a warning, not
// a hard failure — the box rebuild still runs.
type checkError struct{ err error }

func (e checkError) Error() string { return e.err.Error() }
func (e checkError) Unwrap() error { return e.err }

// isDevBuild reports whether v is a non-release build (built from source or via
// `go run`), which self-update can't meaningfully replace.
func isDevBuild(v string) bool {
	switch strings.TrimSpace(v) {
	case "", "dev", "(devel)":
		return true
	}
	return false
}

// normalizeVersion drops a leading "v" and surrounding space so the ldflags
// version ("2.7.2") and a GitHub tag ("v2.7.2") compare equal.
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// latestReleaseTag returns the tag_name of the newest GitHub release (e.g. "v2.7.3").
func latestReleaseTag() (string, error) {
	req, err := http.NewRequest(http.MethodGet, githubLatestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("couldn't check for a newer coop — GitHub returned %s (rate limit or network hiccup?); retry shortly, or reinstall from https://coop.dryga.com", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("couldn't read the latest coop version from GitHub's response — retry shortly, or reinstall from https://coop.dryga.com")
	}
	return rel.TagName, nil
}

// selfUpdate replaces the running coop binary with the latest release, if newer, and
// reports whether it changed anything. A dev build or an already-current binary is a
// no-op (false, nil). An inability to *check* for a release is a checkError (soft); a
// write-permission or install failure is a hard error. The box rebuild that follows in
// cmdUpdate runs in this (pre-update) process; the new binary takes effect next run.
func selfUpdate(out io.Writer) (bool, error) {
	cur := resolveVersion()
	if !releaseVersion(cur) {
		fmt.Fprintln(out, "coop: self-update skipped — this is a dev/source build (install a release first)")
		return false, nil
	}

	exe, err := executablePath()
	if err != nil {
		return false, fmt.Errorf("locate the running coop binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	latest, err := latestReleaseTag()
	if err != nil {
		return false, checkError{err}
	}
	switch compareReleaseVersions(cur, latest) {
	case releaseInvalid:
		return false, checkError{fmt.Errorf("GitHub returned an invalid latest release tag %q", latest)}
	case releaseEqual:
		fmt.Fprintf(out, "coop: already up to date (%s)\n", normalizeVersion(cur))
		return false, nil
	case releaseAhead:
		fmt.Fprintf(out, "coop: %s is newer than GitHub's latest release %s; leaving it unchanged\n", normalizeVersion(cur), normalizeVersion(latest))
		return false, nil
	case releaseBehind:
		// Continue to the verified install below.
	}

	binDir := filepath.Dir(exe)
	if err := dirWritable(binDir); err != nil {
		return false, fmt.Errorf("coop at %s is not writable (%v) — update it with the tool that installed it (your package manager, or reinstall from https://coop.dryga.com)", exe, err)
	}

	fmt.Fprintf(out, "coop: updating %s → %s\n", normalizeVersion(cur), normalizeVersion(latest))
	if err := installRelease(exe, latest); err != nil {
		return false, fmt.Errorf("install %s: %w", latest, err)
	}
	return true, nil
}

// dirWritable reports whether dir accepts new files (so the atomic install can stage a
// temp there), by creating and removing a probe file.
func dirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".coop-writable-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// installRelease downloads the exact GoReleaser archive and checksums for tag,
// verifies the archive locally, then atomically replaces exe.
func installRelease(exe, tag string) error {
	version := normalizeVersion(tag)
	asset := fmt.Sprintf("coop_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	checksums, err := fetchReleaseFile(tag, "checksums.txt", maxReleaseChecksumsBytes)
	if err != nil {
		return fmt.Errorf("fetch checksums.txt: %w", err)
	}
	archive, err := fetchReleaseFile(tag, asset, maxReleaseArchiveBytes)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", asset, err)
	}
	if err := verifyReleaseChecksum(asset, archive, checksums); err != nil {
		return err
	}
	binary, err := releaseBinary(archive)
	if err != nil {
		return fmt.Errorf("read %s: %w", asset, err)
	}
	return replaceExecutable(exe, binary)
}

func fetchReleaseFile(tag, name string, limit int64) ([]byte, error) {
	resp, err := releaseHTTPClient.Get(releaseFileURLFor(tag, name))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return data, nil
}

func verifyReleaseChecksum(asset string, archive, checksums []byte) error {
	want := ""
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		if want != "" {
			return fmt.Errorf("checksums.txt has more than one entry for %s", asset)
		}
		want = fields[0]
	}
	if len(want) != sha256.Size*2 {
		return fmt.Errorf("checksums.txt has no valid SHA-256 entry for %s", asset)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(archive))
	if !strings.EqualFold(want, got) {
		return fmt.Errorf("checksum mismatch for %s", asset)
	}
	return nil
}

func releaseBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	var binary []byte
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != "coop" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("archive member coop is not a regular file")
		}
		if binary != nil {
			return nil, fmt.Errorf("archive contains coop more than once")
		}
		binary, err = io.ReadAll(io.LimitReader(tr, maxReleaseBinaryBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(binary)) > maxReleaseBinaryBytes {
			return nil, fmt.Errorf("archive member coop exceeds %d bytes", maxReleaseBinaryBytes)
		}
	}
	if binary == nil {
		return nil, fmt.Errorf("archive has no coop binary")
	}
	return binary, nil
}

func replaceExecutable(exe string, binary []byte) (retErr error) {
	f, err := os.CreateTemp(filepath.Dir(exe), ".coop-new-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) && retErr == nil {
			retErr = err
		}
	}()
	if _, err := f.Write(binary); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o755); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, exe); err != nil {
		return err
	}
	return nil
}
