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

// The approved `coop update --check` and `coop build` transcripts. --check reads GitHub once and
// the local build stamps; it never asks the runtime, so a report here can be pinned exactly.

// releaseAPI stands in for GitHub's latest-release endpoint.
func releaseAPI(t *testing.T, body string, status int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(stub(&githubLatestURL, srv.URL))
}

// checkProject is a project with a recorded box build, aged as the fixture requires.
type checkProject struct {
	builtDays  int    // how long ago the recorded build happened
	builtBy    string // the coop version stamped on it; "" means this one
	definition string // the box definition hash stamped on it; "" means the current one
	dockerfile string // a project box Dockerfile, which makes the image per-project
	drifted    bool   // the recorded inputs hash no longer matches the files on disk
	unbuilt    bool   // no recorded build at all
}

func (p checkProject) config(t *testing.T) *config.Config {
	t.Helper()
	repo := t.TempDir()
	cfg := &config.Config{BoxHome: t.TempDir(), RepoOverride: repo, BaseImage: "coop-box"}
	if p.dockerfile != "" {
		if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte(p.dockerfile), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if p.unbuilt {
		return cfg
	}
	img := box.ImageForRepo(repo, cfg.BaseImage, "")
	box.StampImageMeta(cfg, img, resolveVersion())
	box.StampImageInputs(cfg, repo, img)
	meta := filepath.Join(cfg.BoxHome, "image-meta", img)
	if p.builtBy != "" || p.definition != "" {
		builtBy, definition := p.builtBy, p.definition
		if builtBy == "" {
			builtBy = resolveVersion()
		}
		if definition == "" {
			definition = "current"
		}
		if err := os.WriteFile(meta, []byte("coop "+builtBy+"\ndef "+definition+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if p.drifted {
		// The stamp stays; the file it was taken from changes. That is exactly the drift a
		// person needs told about.
		if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte(p.dockerfile+"\nRUN echo changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if p.builtDays > 0 {
		when := time.Now().Add(-time.Duration(p.builtDays) * 24 * time.Hour)
		for _, stamp := range []string{meta, filepath.Join(cfg.BoxHome, "image-inputs", img)} {
			if _, err := os.Stat(stamp); err == nil {
				if err := os.Chtimes(stamp, when, when); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return cfg
}

func TestApprovedUpdateCheck(t *testing.T) {
	const projectDockerfile = "FROM coop-box\nRUN true\n"
	cases := []struct {
		fixture string
		version string
		latest  string
		status  int
		project checkProject
		code    int
	}{
		{fixture: "33a-check-current", version: "9.0.0", latest: `{"tag_name":"v9.0.0"}`},
		{fixture: "33b-check-newer-release", version: "9.0.0", latest: `{"tag_name":"v9.0.1"}`,
			project: checkProject{builtDays: 3}},
		{fixture: "33c-check-binary-ahead", version: "9.1.0", latest: `{"tag_name":"v9.0.0"}`},
		{fixture: "33d-check-development-build", version: "9.0.0-187-g1176bf4", latest: `{"tag_name":"v9.0.0"}`},
		{fixture: "33e-check-no-build-record", version: "9.0.0", latest: `{"tag_name":"v9.0.0"}`,
			project: checkProject{unbuilt: true}},
		{fixture: "33g-check-inputs-changed", version: "9.0.0", latest: `{"tag_name":"v9.0.0"}`,
			project: checkProject{dockerfile: projectDockerfile, drifted: true, builtDays: 3}},
		{fixture: "33h-check-definition-skew", version: "9.0.0", latest: `{"tag_name":"v9.0.0"}`,
			project: checkProject{builtDays: 3, builtBy: "v8.9.0", definition: "stale"}},
		{fixture: "33i-check-old-build", version: "9.0.0", latest: `{"tag_name":"v9.0.0"}`,
			project: checkProject{builtDays: 35}},
		{fixture: "33j-check-all-concerns", version: "9.0.0", latest: `{"tag_name":"v9.0.1"}`,
			project: checkProject{dockerfile: projectDockerfile, drifted: true, builtDays: 35,
				builtBy: "v8.9.0", definition: "stale"}},
		{fixture: "33k-check-lookup-failed", version: "9.0.0", status: http.StatusForbidden, code: 1},
		{fixture: "33l-check-invalid-release", version: "9.0.0", latest: `{"tag_name":"not-a-version"}`, code: 1},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			defer stub(&Version, tc.version)()
			releaseAPI(t, tc.latest, tc.status)
			cfg := tc.project.config(t)
			a := &app{cfg: cfg}
			var code int
			out := captureTerminal(t, func() { code, _ = a.cmdUpdateCheck() })
			if code != tc.code {
				t.Errorf("exit = %d, want %d", code, tc.code)
			}
			assertApprovedOutput(t, tc.fixture, normalizeImage(cfg, out))
		})
	}

	// With no project folder there is no build record to read, which is a different answer from
	// an image that was never built.
	t.Run("33f-check-no-project", func(t *testing.T) {
		defer stub(&Version, "9.0.0")()
		releaseAPI(t, `{"tag_name":"v9.0.0"}`, 0)
		a := &app{cfg: &config.Config{BoxHome: t.TempDir(), RepoOverride: filepath.Join(t.TempDir(), "missing")}}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUpdateCheck() })
		if code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		assertApprovedOutput(t, "33f-check-no-project", out)
	})
}

// normalizeImage replaces the per-checkout project image name with the one the transcripts
// record: its hash is a property of the path, not of the copy.
func normalizeImage(cfg *config.Config, out string) string {
	img := box.ImageForRepo(cfg.RepoOverride, cfg.BaseImage, "")
	if img == cfg.BaseImage {
		return out
	}
	return strings.ReplaceAll(out, img, "coop-atlas")
}
