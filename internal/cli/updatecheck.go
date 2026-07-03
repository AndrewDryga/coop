package cli

// The once-a-day update-available notice. The first TTY command of a day fires a
// background fetch of the latest release tag and prints one parting line if it's newer
// than the running binary (gh's shape: the command never waits on the network — a
// short-lived command misses the result, and a later run prints it from the cached
// file). BoxHome/update-check is both the day gate (its mtime) and the cache (its
// content, the last-fetched tag). Best-effort everywhere: any failure just means no
// notice. COOP_NO_UPDATE_CHECK opts out; `coop update --check` is the on-demand form.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

func updateCheckPath(cfg *config.Config) string {
	return filepath.Join(cfg.BoxHome, "update-check")
}

// updateCheckDue reports whether the daily check should run: no gate file yet, or the
// last attempt was over a day ago (its mtime — touched at each attempt, success or not).
func updateCheckDue(path string, now time.Time) bool {
	fi, err := os.Stat(path)
	return err != nil || now.Sub(fi.ModTime()) >= 24*time.Hour
}

// touchUpdateCheck marks an attempt NOW while keeping the previously cached tag readable,
// so a run that exits before the fetch lands still leaves yesterday's answer in place.
func touchUpdateCheck(path string) {
	now := time.Now()
	if os.Chtimes(path, now, now) == nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o755) == nil {
		_ = os.WriteFile(path, nil, 0o644)
	}
}

// cacheLatest stores the fetched tag via temp+rename, so a process exiting mid-write
// can't leave a torn tag for the next run to compare against.
func cacheLatest(path, tag string) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".update-check-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	_, werr := tmp.WriteString(tag + "\n")
	if cerr := tmp.Close(); werr != nil || cerr != nil {
		_ = os.Remove(name)
		return
	}
	if os.Rename(name, path) != nil {
		_ = os.Remove(name)
	}
}

func cachedLatest(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// releaseVersion reports whether v is a clean release build — the only kind the notice
// compares. A dev build, or a git-describe/+dirty suffix (a source build AHEAD of the
// latest release), must never be told to "update".
func releaseVersion(v string) bool {
	return !isDevBuild(v) && !strings.ContainsAny(v, "-+")
}

// versionLess reports whether a < b as dotted numeric versions ("2.10.1" < "3.0.0",
// and "9.9" < "10.0" — which a string compare gets wrong). When the deciding part is
// non-numeric the comparison is a guess, so it reads as not-less (no notice on a guess).
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		an, bn := 0, 0
		var err error
		if i < len(as) {
			if an, err = strconv.Atoi(as[i]); err != nil {
				return false
			}
		}
		if i < len(bs) {
			if bn, err = strconv.Atoi(bs[i]); err != nil {
				return false
			}
		}
		if an != bn {
			return an < bn
		}
	}
	return false
}

// updateNotice is the parting line for a newer release, or "" when there's nothing to
// say (no cached/fetched tag, a non-release build, or already current-or-ahead).
func updateNotice(cur, latest string) string {
	if latest == "" || !releaseVersion(cur) {
		return ""
	}
	c, l := normalizeVersion(cur), normalizeVersion(latest)
	if !versionLess(c, l) {
		return ""
	}
	return "a newer coop is available: v" + c + " → v" + l + " — run 'coop update' (or set COOP_NO_UPDATE_CHECK=1)"
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
			ui.Info("%s", msg)
		}
	}
}
