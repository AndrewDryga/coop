package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestUpdateCheckDue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-check")
	now := time.Now()
	if !updateCheckDue(path, now) {
		t.Error("no gate file yet — the first check is due")
	}
	touchUpdateCheck(path)
	if updateCheckDue(path, now) {
		t.Error("just checked — not due again today")
	}
	if !updateCheckDue(path, now.Add(25*time.Hour)) {
		t.Error("a day later — due again")
	}
}

// touchUpdateCheck marks an attempt without losing the previously cached tag, so a
// short-lived run can still print yesterday's fetched answer.
func TestTouchKeepsCachedTag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-check")
	cacheLatest(path, "v9.9.9")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	touchUpdateCheck(path)
	if updateCheckDue(path, time.Now()) {
		t.Error("touch must refresh the day gate")
	}
	if got := cachedLatest(path); got != "v9.9.9" {
		t.Errorf("touch must keep the cached tag, got %q", got)
	}
}

func TestCacheLatestRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-check")
	if got := cachedLatest(path); got != "" {
		t.Errorf("missing cache reads empty, got %q", got)
	}
	cacheLatest(path, "v3.1.0")
	if got := cachedLatest(path); got != "v3.1.0" {
		t.Errorf("cachedLatest = %q, want v3.1.0", got)
	}
}

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2.10.1", "3.0.0", true},
		{"9.9", "10.0", true}, // numeric, not lexicographic
		{"3.0.0", "3.0.0", false},
		{"3.1.0", "3.0.9", false},
		{"3.0", "3.0.1", true},    // shorter version pads with zeros
		{"3.0.x", "3.0.9", false}, // decision reaches a non-numeric part → a guess → not-less
		{"3.0.x", "4.0", true},    // ...but an earlier numeric part can still decide
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestReleaseVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"2.10.1":                    true,
		"v3.0.0":                    true,
		"dev":                       false,
		"(devel)":                   false,
		"":                          false,
		"2.10.1-254-g3d300c7-dirty": false, // a source build ahead of the release
		"3.0.0+dirty":               false,
	} {
		if got := releaseVersion(v); got != want {
			t.Errorf("releaseVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestUpdateNotice(t *testing.T) {
	// Newer release → one line naming both versions and the command.
	msg := updateNotice("2.10.1", "v3.0.0")
	for _, want := range []string{"v2.10.1", "v3.0.0", "coop update", "COOP_NO_UPDATE_CHECK"} {
		if !strings.Contains(msg, want) {
			t.Errorf("notice missing %q: %q", want, msg)
		}
	}
	// Nothing to say: current, ahead, no data, or a dev build.
	for name, args := range map[string][2]string{
		"already current": {"3.0.0", "v3.0.0"},
		"ahead of latest": {"3.1.0", "v3.0.0"},
		"no cached tag":   {"3.0.0", ""},
		"dev build":       {"2.10.1-254-g3d300c7-dirty", "v3.0.0"},
	} {
		if msg := updateNotice(args[0], args[1]); msg != "" {
			t.Errorf("%s: want no notice, got %q", name, msg)
		}
	}
}

// `coop update --check` reports the binary and the box from local stamps, changing nothing —
// and needs no container runtime.
func TestCmdUpdateCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v9.0.0"}`)
	}))
	defer srv.Close()
	defer stub(&githubLatestURL, srv.URL)()
	defer stub(&Version, "2.9.0")()

	repo := t.TempDir()
	cfg := &config.Config{BoxHome: t.TempDir(), RepoOverride: repo, BaseImage: "coop-box"}
	// Stamp the base image as built long ago by an older definition → age + skew lines.
	box.StampImageMeta(cfg, "coop-box", "v2.0.0")
	metaPath := filepath.Join(cfg.BoxHome, "image-meta", "coop-box")
	if err := os.WriteFile(metaPath, []byte("coop v2.0.0\ndef stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(metaPath, old, old); err != nil {
		t.Fatal(err)
	}

	a := &app{cfg: cfg}
	var code int
	var err error
	joined := captureStderr(t, func() {
		code, err = a.cmdUpdateCheck()
	})
	if code != 0 || err != nil {
		t.Fatalf("cmdUpdateCheck: code=%d err=%v", code, err)
	}
	for _, want := range []string{
		"v2.9.0 → v9.0.0",                  // the binary line
		"built 40 days ago",                // the box build age
		"built by coop v2.0.0", "days old", // skew + age nudges
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("--check output missing %q:\n%s", want, joined)
		}
	}

	// The check itself failing (network) is a real error for an explicit --check.
	srv.Close()
	if code, err := a.cmdUpdateCheck(); code == 0 || err == nil {
		t.Errorf("unreachable GitHub: want a non-zero exit + error, got code=%d err=%v", code, err)
	}
}
