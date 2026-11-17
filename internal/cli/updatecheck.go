package cli

// The once-a-day update-available notice. The first TTY command of a day fires a
// background fetch of the latest release tag and prints one parting line if it's newer
// than the running binary (gh's shape: the command never waits on the network — a
// short-lived command misses the result, and the next due run prints it from the cached
// file). LocalHome/update-check is both the day gate (its mtime) and the cache (its
// content, the last-fetched tag); only a fetch that finished writes it, so a day whose
// first commands all exit before GitHub answers tries again rather than skipping a day.
// update-check.attempt spaces those tries out. Best-effort everywhere: any failure just
// means no notice. COOP_NO_UPDATE_CHECK opts out; `coop update --check` is the on-demand form.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

type releaseRelation uint8

const (
	releaseInvalid releaseRelation = iota
	releaseBehind
	releaseEqual
	releaseAhead
)

func updateCheckPath(cfg *config.Config) string {
	return filepath.Join(cfg.LocalHome(), "update-check")
}

// updateAttemptBackoff keeps coops started together from fetching together; an attempt that
// never finished is tried again after it, not a day later.
const updateAttemptBackoff = 10 * time.Minute

func updateAttemptPath(path string) string { return path + ".attempt" }

// updateCheckDue reports whether the daily check should run: the last FINISHED fetch (the
// cache's mtime) is missing or over a day old, and no attempt started within the backoff.
func updateCheckDue(path string, now time.Time) bool {
	if fi, err := os.Stat(path); err == nil && now.Sub(fi.ModTime()) < 24*time.Hour {
		return false
	}
	fi, err := os.Stat(updateAttemptPath(path))
	return err != nil || now.Sub(fi.ModTime()) >= updateAttemptBackoff
}

// touchUpdateCheck marks an attempt NOW, beside the cache, so the previously cached tag and
// its day gate stay as they were until a fetch lands.
func touchUpdateCheck(path string) {
	attempt := updateAttemptPath(path)
	now := time.Now()
	if os.Chtimes(attempt, now, now) == nil {
		return
	}
	if os.MkdirAll(filepath.Dir(attempt), 0o755) == nil {
		_ = os.WriteFile(attempt, nil, 0o644)
	}
}

// cacheLatest stores the fetched tag via temp+rename, so a process exiting mid-write
// can't leave a torn tag for the next run to compare against, and so stamps the day gate.
func cacheLatest(path, tag string) {
	_ = config.WriteFileAtomic(path, []byte(tag+"\n"))
}

func cachedLatest(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// releaseVersion reports whether v is a clean x.y.z release. A dev build or a
// git-describe/+dirty suffix must never be guessed older than a published release.
func releaseVersion(v string) bool {
	_, ok := releaseParts(v)
	return ok
}

func releaseParts(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(normalizeVersion(v), ".")
	if len(parts) != len(out) {
		return out, false
	}
	for i, part := range parts {
		if part == "" {
			return out, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// compareReleaseVersions is the one ordering decision used by passive notices,
// explicit checks, and installation. An invalid side is never ordered by guess.
func compareReleaseVersions(current, latest string) releaseRelation {
	c, cok := releaseParts(current)
	l, lok := releaseParts(latest)
	if !cok || !lok {
		return releaseInvalid
	}
	for i := range c {
		switch {
		case c[i] < l[i]:
			return releaseBehind
		case c[i] > l[i]:
			return releaseAhead
		}
	}
	return releaseEqual
}

// updateNotice is the parting line for a newer release, or "" when there's nothing to
// say (no cached/fetched tag, a non-release build, or already current-or-ahead).
func updateNotice(cur, latest string) string {
	if compareReleaseVersions(cur, latest) != releaseBehind {
		return ""
	}
	c, l := normalizeVersion(cur), normalizeVersion(latest)
	return "Update Coop from v" + c + " to v" + l + ". Run 'coop update' (or set COOP_NO_UPDATE_CHECK=1)"
}

// startUpdateCheck begins the daily background check when it's due and returns the
// finisher Main defers: it prints the one notice line after the command's own output.
// Silent no-op for: `coop update` (you're updating) and `coop acp` (stdio protocol),
// opted-out configs, dev/source builds, non-TTY runs, and days already checked.
func startUpdateCheck(cfg *config.Config, argv []string) func() {
	if len(argv) > 0 && (argv[0] == "update" || argv[0] == "acp") {
		return func() {}
	}
	cur := resolveVersion()
	if cfg.NoUpdateCheck || !releaseVersion(cur) || !ui.IsTerminal(os.Stdout) || !ui.IsTerminal(os.Stderr) {
		return func() {}
	}
	path := updateCheckPath(cfg)
	if !updateCheckDue(path, time.Now()) {
		return func() {}
	}
	touchUpdateCheck(path) // mark the attempt first, so parallel coops don't re-check today
	fresh := make(chan string, 1)
	go func() {
		if tag, err := latestReleaseTag(); err == nil {
			cacheLatest(path, tag)
			fresh <- tag
		}
	}()
	return func() {
		latest := ""
		select {
		case latest = <-fresh: // the fetch finished during this command
		default:
			latest = cachedLatest(path) // fall back to a previous run's answer
		}
		if msg := updateNotice(cur, latest); msg != "" {
			ui.Note("%s", msg)
		}
	}
}
