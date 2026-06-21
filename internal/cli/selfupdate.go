package cli

// Self-update replaces the running coop binary with the latest GitHub release. It
// deliberately reuses install.sh — download + checksum + cosign verification,
// os/arch detection, and the atomic running-binary-safe install — instead of
// re-implementing that security-critical chain in Go. There is one place that
// knows how to put a verified coop on disk; this fetches it (pinned to the target
// tag) and runs it.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// These are vars (not consts) so tests can point them at an httptest server.
var (
	githubLatestURL = "https://api.github.com/repos/AndrewDryga/coop/releases/latest"

	installScriptURLFor = func(tag string) string {
		return "https://raw.githubusercontent.com/AndrewDryga/coop/" + tag + "/install.sh"
	}

	// executablePath resolves the running binary; a var so tests can stub it.
	executablePath = os.Executable

	updateHTTPClient = &http.Client{Timeout: 30 * time.Second}
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

func sameVersion(a, b string) bool { return normalizeVersion(a) == normalizeVersion(b) }

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
		return "", fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("GitHub API response had no tag_name")
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
	if isDevBuild(cur) {
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
	if sameVersion(cur, latest) {
		fmt.Fprintf(out, "coop: already up to date (%s)\n", normalizeVersion(cur))
		return false, nil
	}

	binDir := filepath.Dir(exe)
	if err := dirWritable(binDir); err != nil {
		return false, fmt.Errorf("coop at %s is not writable (%v) — update it with the tool that installed it (your package manager, or re-run install.sh)", exe, err)
	}

	fmt.Fprintf(out, "coop: updating %s → %s\n", normalizeVersion(cur), normalizeVersion(latest))
	if err := runInstaller(out, binDir, latest); err != nil {
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

// runInstaller fetches install.sh at the target tag and runs it pointed at binDir,
// pinned to that version and skipping the box build (coop update does that itself).
// install.sh performs the verified download and the atomic, running-binary-safe replace.
func runInstaller(out io.Writer, binDir, tag string) error {
	resp, err := updateHTTPClient.Get(installScriptURLFor(tag))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch installer for %s: %s", tag, resp.Status)
	}
	script, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "coop-selfupdate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, script, 0o755); err != nil {
		return err
	}

	cmd := exec.Command("sh", path)
	cmd.Env = append(os.Environ(),
		"COOP_BIN_DIR="+binDir,
		"COOP_VERSION="+tag,
		"COOP_NO_BUILD=1",
	)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}
