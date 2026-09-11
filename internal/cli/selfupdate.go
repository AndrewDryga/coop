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
	"errors"
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
		return "", fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub returned %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", errors.New("GitHub did not return a valid release version")
	}
	if rel.TagName == "" {
		return "", errors.New("GitHub did not return a valid release version")
	}
	return rel.TagName, nil
}

// selfUpdateOutcome is what the binary half of an update DID — which is what its caller has to
// report, and the one thing it must not guess at.
type selfUpdateOutcome int

const (
	selfUpdateDev       selfUpdateOutcome = iota // a dev/source build; self-update does not replace it
	selfUpdateCurrent                            // already the latest release
	selfUpdateAhead                              // newer than the latest release; kept
	selfUpdateInstalled                          // replaced with a newer release
)

// selfUpdateResult carries the versions involved so the caller can name both without asking
// GitHub a second time.
type selfUpdateResult struct {
	Outcome selfUpdateOutcome
	Current string // this binary's version, normalized (no leading v)
	Latest  string // the latest release, normalized; "" when it was never resolved
}

// installFailure is a hard binary-install failure, carrying the sentence a person reads and
// whether the binary on disk is provably untouched. Unchanged is only true for a failure that
// happens BEFORE the atomic rename — nothing after it can undo an installation that happened.
type installFailure struct {
	reason    string
	unchanged bool
	err       error
}

func (e *installFailure) Error() string { return e.reason }
func (e *installFailure) Unwrap() error { return e.err }

// selfUpdate replaces the running coop binary with the latest release, if newer, and reports
// what it did. A dev build, an already-current binary and a binary ahead of the release are all
// no-ops. An inability to *check* for a release is a checkError (soft — the box rebuild still
// runs); a write-permission or install failure is an installFailure (hard). The box rebuild that
// follows in cmdUpdate runs in this (pre-update) process; the new binary takes effect next run.
func selfUpdate() (selfUpdateResult, error) {
	cur := resolveVersion()
	if !releaseVersion(cur) {
		return selfUpdateResult{Outcome: selfUpdateDev, Current: cur}, nil
	}
	result := selfUpdateResult{Current: normalizeVersion(cur)}

	exe, err := executablePath()
	if err != nil {
		return result, &installFailure{reason: "Could not locate the running Coop binary: " + err.Error() + ".", unchanged: true, err: err}
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	latest, err := latestReleaseTag()
	if err != nil {
		return result, checkError{err}
	}
	result.Latest = normalizeVersion(latest)
	switch compareReleaseVersions(cur, latest) {
	case releaseInvalid:
		return result, checkError{errors.New("GitHub did not return a valid release version")}
	case releaseEqual:
		result.Outcome = selfUpdateCurrent
		return result, nil
	case releaseAhead:
		result.Outcome = selfUpdateAhead
		return result, nil
	case releaseBehind:
		// Continue to the verified install below.
	}

	if err := dirWritable(filepath.Dir(exe)); err != nil {
		return result, &installFailure{reason: exe + " could not be replaced: " + osCause(err) + ".", unchanged: true, err: err}
	}
	if err := installRelease(exe, latest); err != nil {
		return result, err
	}
	result.Outcome = selfUpdateInstalled
	return result, nil
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
// verifies the archive locally, then atomically replaces exe. Every failure before the rename
// leaves the installed binary untouched and says so; only the rename itself can change it.
func installRelease(exe, tag string) error {
	version := normalizeVersion(tag)
	asset := fmt.Sprintf("coop_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	fail := func(reason string, err error) error {
		return &installFailure{reason: reason, unchanged: true, err: err}
	}
	checksums, err := fetchReleaseFile(tag, "checksums.txt", maxReleaseChecksumsBytes)
	if err != nil {
		return fail("Could not download the release checksums: "+err.Error()+".", err)
	}
	archive, err := fetchReleaseFile(tag, asset, maxReleaseArchiveBytes)
	if err != nil {
		return fail("Could not download "+asset+": "+err.Error()+".", err)
	}
	if err := verifyReleaseChecksum(asset, archive, checksums); err != nil {
		return fail(sentence(err.Error()), err)
	}
	binary, err := releaseBinary(archive)
	if err != nil {
		return fail(sentence(err.Error()), err)
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
			return fmt.Errorf("the release checksums contain more than one entry for %s", asset)
		}
		want = fields[0]
	}
	if len(want) != sha256.Size*2 {
		return fmt.Errorf("the release checksums contain no valid entry for %s", asset)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(archive))
	if !strings.EqualFold(want, got) {
		return errors.New("the downloaded release did not match its published checksum")
	}
	return nil
}

func releaseBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("the downloaded release archive could not be read: %w", err)
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
			return nil, fmt.Errorf("the downloaded release archive could not be read: %w", err)
		}
		if hdr.Name != "coop" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, errors.New("the release archive's coop entry is not a regular file")
		}
		if binary != nil {
			return nil, errors.New("the release archive contains more than one Coop binary")
		}
		binary, err = io.ReadAll(io.LimitReader(tr, maxReleaseBinaryBytes+1))
		if err != nil {
			return nil, fmt.Errorf("the downloaded release archive could not be read: %w", err)
		}
		if int64(len(binary)) > maxReleaseBinaryBytes {
			return nil, errors.New("the release archive's Coop binary exceeds the supported size")
		}
	}
	if binary == nil {
		return nil, errors.New("the release archive contains no Coop binary")
	}
	return binary, nil
}

func replaceExecutable(exe string, binary []byte) (retErr error) {
	// Everything up to the rename is staging: it can fail freely, and the binary on disk is the
	// one that was there before. Cleanup AFTER a successful rename cannot take that back, so its
	// failure is reported without the "unchanged" claim.
	prepare := func(err error) error {
		return &installFailure{reason: "Could not prepare the new Coop binary: " + osCause(err) + ".", unchanged: true, err: err}
	}
	f, err := os.CreateTemp(filepath.Dir(exe), ".coop-new-*")
	if err != nil {
		return prepare(err)
	}
	name := f.Name()
	installed := false
	defer func() {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) && retErr == nil {
			retErr = &installFailure{reason: "Could not clean up the staged Coop binary: " + osCause(err) + ".", unchanged: !installed, err: err}
		}
	}()
	if _, err := f.Write(binary); err != nil {
		_ = f.Close()
		return prepare(err)
	}
	if err := f.Chmod(0o755); err != nil {
		_ = f.Close()
		return prepare(err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return prepare(err)
	}
	if err := f.Close(); err != nil {
		return prepare(err)
	}
	if err := os.Rename(name, exe); err != nil {
		return &installFailure{reason: "Could not replace " + exe + ": " + osCause(err) + ".", unchanged: true, err: err}
	}
	installed = true
	return nil
}
