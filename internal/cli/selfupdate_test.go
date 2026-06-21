package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stub sets *p to v and returns a restore func, so tests can point package vars
// (Version, the URLs, executablePath) at fixtures: `defer stub(&Version, "2.7.2")()`.
func stub[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

func TestVersionHelpers(t *testing.T) {
	for _, v := range []string{"", "dev", "(devel)", "  dev  "} {
		if !isDevBuild(v) {
			t.Errorf("isDevBuild(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"2.7.2", "v2.7.2"} {
		if isDevBuild(v) {
			t.Errorf("isDevBuild(%q) = true, want false", v)
		}
	}
	if !sameVersion("2.7.2", "v2.7.2") {
		t.Error(`sameVersion("2.7.2","v2.7.2") = false, want true (leading v normalized)`)
	}
	if sameVersion("2.7.1", "v2.7.2") {
		t.Error("sameVersion of different versions = true, want false")
	}
}

func TestLatestReleaseTag(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v2.7.3","name":"2.7.3"}`)
		}))
		defer srv.Close()
		defer stub(&githubLatestURL, srv.URL)()
		got, err := latestReleaseTag()
		if err != nil {
			t.Fatal(err)
		}
		if got != "v2.7.3" {
			t.Errorf("tag = %q, want v2.7.3", got)
		}
	})
	t.Run("non-200 errors", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		defer stub(&githubLatestURL, srv.URL)()
		if _, err := latestReleaseTag(); err == nil {
			t.Error("want an error on a non-200 response")
		}
	})
	t.Run("missing tag errors", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{}`)
		}))
		defer srv.Close()
		defer stub(&githubLatestURL, srv.URL)()
		if _, err := latestReleaseTag(); err == nil {
			t.Error("want an error when the response has no tag_name")
		}
	})
}

// TestRunInstaller proves runInstaller fetches the installer and runs it with the
// pinned version, target bin dir, and box-build skip — by serving a fake install.sh
// that records the env it saw.
func TestRunInstaller(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// %s here is shell (printf), not Go — write raw so vet doesn't read it as a directive.
		io.WriteString(w, "#!/bin/sh\nprintf '%s|%s|%s' \"$COOP_BIN_DIR\" \"$COOP_VERSION\" \"$COOP_NO_BUILD\" > \"$COOP_BIN_DIR/marker\"\n")
	}))
	defer srv.Close()
	defer stub(&installScriptURLFor, func(tag string) string { return srv.URL + "/" + tag })()

	binDir := t.TempDir()
	var out bytes.Buffer
	if err := runInstaller(&out, binDir, "v9.9.9"); err != nil {
		t.Fatalf("runInstaller: %v (out=%s)", err, out.String())
	}
	got, err := os.ReadFile(filepath.Join(binDir, "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if want := binDir + "|v9.9.9|1"; string(got) != want {
		t.Errorf("installer env = %q, want %q", got, want)
	}
}

func TestSelfUpdate(t *testing.T) {
	t.Run("dev build is a no-op without network", func(t *testing.T) {
		defer stub(&Version, "dev")()
		defer stub(&githubLatestURL, "http://127.0.0.1:1/must-not-be-called")()
		var out bytes.Buffer
		changed, err := selfUpdate(&out)
		if changed || err != nil {
			t.Fatalf("dev build: changed=%v err=%v", changed, err)
		}
		if !strings.Contains(out.String(), "dev/source build") {
			t.Errorf("missing dev note, got %q", out.String())
		}
	})

	t.Run("already current does not run the installer", func(t *testing.T) {
		defer stub(&Version, "2.7.3")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v2.7.3"}`)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		defer stub(&installScriptURLFor, func(string) string {
			t.Error("installer must not run when already current")
			return ""
		})()
		exe := filepath.Join(t.TempDir(), "coop")
		mustWrite(t, exe, "x")
		defer stub(&executablePath, func() (string, error) { return exe, nil })()

		var out bytes.Buffer
		changed, err := selfUpdate(&out)
		if changed || err != nil {
			t.Fatalf("current: changed=%v err=%v", changed, err)
		}
		if !strings.Contains(out.String(), "up to date") {
			t.Errorf("missing up-to-date note, got %q", out.String())
		}
	})

	t.Run("newer release runs the installer", func(t *testing.T) {
		if _, err := exec.LookPath("sh"); err != nil {
			t.Skip("sh not available")
		}
		defer stub(&Version, "2.7.2")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v2.7.3"}`)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		script := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "#!/bin/sh\nprintf done > \"$COOP_BIN_DIR/marker\"\n")
		}))
		defer script.Close()
		defer stub(&installScriptURLFor, func(tag string) string { return script.URL + "/" + tag })()
		exeDir := t.TempDir()
		exe := filepath.Join(exeDir, "coop")
		mustWrite(t, exe, "old")
		defer stub(&executablePath, func() (string, error) { return exe, nil })()

		var out bytes.Buffer
		changed, err := selfUpdate(&out)
		if !changed || err != nil {
			t.Fatalf("newer: changed=%v err=%v out=%s", changed, err, out.String())
		}
		if _, err := os.Stat(filepath.Join(exeDir, "marker")); err != nil {
			t.Errorf("installer did not run: %v", err)
		}
		if !strings.Contains(out.String(), "updating 2.7.2 → 2.7.3") {
			t.Errorf("missing update note, got %q", out.String())
		}
	})

	t.Run("check failure is soft (checkError)", func(t *testing.T) {
		defer stub(&Version, "2.7.2")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		var out bytes.Buffer
		_, err := selfUpdate(&out)
		var ce checkError
		if !errors.As(err, &ce) {
			t.Fatalf("want a checkError (soft), got %v", err)
		}
	})

	t.Run("unwritable location is a hard error", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores file permissions")
		}
		defer stub(&Version, "2.7.2")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v2.7.3"}`)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		roDir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(roDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) }) // so TempDir cleanup can remove it
		exe := filepath.Join(roDir, "coop")
		defer stub(&executablePath, func() (string, error) { return exe, nil })()

		var out bytes.Buffer
		_, err := selfUpdate(&out)
		if err == nil {
			t.Fatal("want an error for an unwritable install location")
		}
		var ce checkError
		if errors.As(err, &ce) {
			t.Error("unwritable should be a hard error, not a checkError")
		}
		if !strings.Contains(err.Error(), "not writable") {
			t.Errorf("err = %v, want it to mention 'not writable'", err)
		}
	})
}

func TestParseUpdateFlags(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		self, box bool
		wantErr   bool
	}{
		{"none", nil, false, false, false},
		{"self-only", []string{"--self-only"}, true, false, false},
		{"box-only", []string{"--box-only"}, false, true, false},
		{"both is an error", []string{"--self-only", "--box-only"}, false, false, true},
		{"unknown flag", []string{"--wat"}, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			self, box, err := parseUpdateFlags(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			if self != c.self || box != c.box {
				t.Errorf("self=%v box=%v, want %v/%v", self, box, c.self, c.box)
			}
		})
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
