package cli

// Self-update replaces the running coop binary with a newer GitHub release. It
// downloads the versioned GoReleaser archive and checksum as data, verifies them
// locally, and never executes a downloaded installer. When cosign is installed it also
// verifies the checksums' Sigstore signature, as install.sh does; cosign stays optional.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

// These are vars (not consts) so tests can point them at an httptest server.
var (
	githubLatestURL = "https://api.github.com/repos/AndrewDryga/coop/releases/latest"

	releaseFileURLFor = func(tag, name string) string {
		return "https://github.com/AndrewDryga/coop/releases/download/" + tag + "/" + name
	}

	// executablePath resolves the running binary; a var so tests can stub it.
	executablePath = os.Executable
	updateExec     = syscall.Exec

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

// releaseSourceMissing is GitHub answering 404 Not Found for the latest release: nothing is
// published at the address this build asks. Waiting does not change that answer; a current build
// from the installer, which asks the right address, does.
type releaseSourceMissing struct{ url string }

func (e *releaseSourceMissing) Error() string { return "GitHub returned 404 Not Found for " + e.url }

// reinstallCommand is the documented installer, which always fetches the current official build.
const reinstallCommand = "curl -fsSL https://coop.dryga.com/install.sh | sh"

// releaseCheckReason is the cause a failed release lookup prints, plus the action that fixes it
// when waiting will not: a missing release source names the address asked and how to reinstall.
// Any other failure (offline, a rate limit, a GitHub outage) may pass, so its fix is left to the
// caller, which knows what to retry.
func releaseCheckReason(err error) (reason, fix string) {
	var missing *releaseSourceMissing
	if errors.As(err, &missing) {
		return "The release source was not found (GitHub returned 404 Not Found):\n" + missing.url,
			"Reinstall Coop with: " + reinstallCommand
	}
	return sentence(firstLine(err)), ""
}

// releaseCheckFailure reports a release lookup that failed, with retry as the command to run
// again when the failure may pass.
func releaseCheckFailure(err error, retry string) error {
	reason, fix := releaseCheckReason(err)
	if fix == "" {
		fix = "Try " + retry + " again later."
	}
	return reported("Could not check for a Coop update", reason, fix)
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
	if resp.StatusCode == http.StatusNotFound {
		return "", &releaseSourceMissing{url: githubLatestURL}
	}
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
	Outcome    selfUpdateOutcome
	Current    string // this binary's version, normalized (no leading v)
	Latest     string // the latest release, normalized; "" when it was never resolved
	Executable string // resolved path actually replaced; only set after a verified install
	Signed     bool   // cosign verified the release signature; false when cosign is not installed
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
// follows in cmdUpdate continues in the installed executable, so its own image definitions apply.
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
	if exe, err = filepath.Abs(exe); err != nil {
		return result, &installFailure{reason: "Could not resolve the running Coop binary: " + err.Error() + ".", unchanged: true, err: err}
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
	signed, err := installRelease(exe, latest)
	if err != nil {
		return result, err
	}
	result.Outcome = selfUpdateInstalled
	result.Signed = signed
	result.Executable = exe
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
// signed reports whether cosign verified checksums.txt, which happens only when cosign is installed.
func installRelease(exe, tag string) (signed bool, err error) {
	version := normalizeVersion(tag)
	asset := fmt.Sprintf("coop_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	fail := func(reason string, err error) error {
		return &installFailure{reason: reason, unchanged: true, err: err}
	}
	checksums, err := fetchReleaseFile(tag, "checksums.txt", maxReleaseChecksumsBytes)
	if err != nil {
		return false, fail("Could not download the release checksums: "+err.Error()+".", err)
	}
	if cosign, lookErr := exec.LookPath("cosign"); lookErr == nil {
		if err := verifyReleaseSignature(cosign, tag, checksums); err != nil {
			return false, fail(sentence(err.Error()), err)
		}
		signed = true
	}
	archive, err := fetchReleaseFile(tag, asset, maxReleaseArchiveBytes)
	if err != nil {
		return false, fail("Could not download "+asset+": "+err.Error()+".", err)
	}
	if err := verifyReleaseChecksum(asset, archive, checksums); err != nil {
		return false, fail(sentence(err.Error()), err)
	}
	binary, err := releaseBinary(archive)
	if err != nil {
		return false, fail(sentence(err.Error()), err)
	}
	return signed, replaceExecutable(exe, binary)
}

// releaseSignatureTimeout bounds cosign, which may fetch Sigstore's trust root over the network.
const releaseSignatureTimeout = 2 * time.Minute

// verifyReleaseSignature runs cosign on checksums.txt and its Sigstore bundle with the identity
// install.sh checks: the release workflow for exactly this tag. A missing bundle is a refusal,
// never a skip: every release a running Coop can update to is signed (all tags from v2.2.2 on),
// and an attacker who can replace the archive and checksums.txt can just as easily drop the bundle.
func verifyReleaseSignature(cosign, tag string, checksums []byte) error {
	bundle, err := fetchReleaseFile(tag, "checksums.txt.bundle", maxReleaseChecksumsBytes)
	if err != nil {
		return fmt.Errorf("could not download the release signature (checksums.txt.bundle): %w; releases are signed, so the update stopped", err)
	}
	dir, err := os.MkdirTemp("", "coop-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	sums, sig := filepath.Join(dir, "checksums.txt"), filepath.Join(dir, "checksums.txt.bundle")
	if err := os.WriteFile(sums, checksums, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(sig, bundle, 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseSignatureTimeout)
	defer cancel()
	args := []string{"verify-blob", sums,
		"--bundle", sig,
		"--certificate-identity", "https://github.com/AndrewDryga/coop/.github/workflows/release.yml@refs/tags/" + tag,
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com"}
	cmd := exec.CommandContext(ctx, cosign, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		reason := "cosign could not verify the release signature, so the update stopped"
		if line, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); line != "" {
			reason += ": " + ui.SafeInline(strings.TrimSpace(line))
		}
		return errors.New(reason)
	}
	return nil
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
