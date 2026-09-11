package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
	for name, tc := range map[string]struct {
		current, latest string
		want            releaseRelation
	}{
		"behind":           {"2.7.2", "v2.7.3", releaseBehind},
		"equal":            {"2.7.3", "v2.7.3", releaseEqual},
		"ahead":            {"3.1.0", "v3.0.0", releaseAhead},
		"dev current":      {"dev", "v3.0.0", releaseInvalid},
		"dirty current":    {"3.0.0+dirty", "v3.0.0", releaseInvalid},
		"malformed latest": {"3.0.0", "latest", releaseInvalid},
		"short version":    {"3.0", "3.0.1", releaseInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			if got := compareReleaseVersions(tc.current, tc.latest); got != tc.want {
				t.Errorf("compareReleaseVersions(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
			}
		})
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

func TestInstallRelease(t *testing.T) {
	archive := testReleaseArchive(t, "new binary", "coop")
	asset := fmt.Sprintf("coop_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), asset)
	var paths []string
	srv := testReleaseServer(t, archive, []byte(sum), &paths)
	defer srv.Close()
	defer stub(&releaseFileURLFor, func(tag, name string) string {
		return srv.URL + "/" + tag + "/" + name
	})()

	exe := filepath.Join(t.TempDir(), "renamed-coop")
	mustWrite(t, exe, "old binary")
	before, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := installRelease(exe, "v9.9.9"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Errorf("installed bytes = %q", got)
	}
	after, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("replacement reused the old executable inode; want atomic rename")
	}
	if after.Mode().Perm() != 0o755 {
		t.Errorf("installed mode = %o, want 755", after.Mode().Perm())
	}
	wantPaths := []string{"/v9.9.9/checksums.txt", "/v9.9.9/" + asset}
	if fmt.Sprint(paths) != fmt.Sprint(wantPaths) {
		t.Errorf("release requests = %v, want %v", paths, wantPaths)
	}
}

func TestInstallReleaseFailuresKeepExecutable(t *testing.T) {
	good := testReleaseArchive(t, "new binary", "coop")
	missingBinary := testReleaseArchive(t, "readme", "README.md")
	asset := fmt.Sprintf("coop_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	for name, tc := range map[string]struct {
		archive   []byte
		checksums func([]byte) []byte
	}{
		"missing checksum":  {good, func([]byte) []byte { return []byte("deadbeef  other.tar.gz\n") }},
		"checksum mismatch": {good, func([]byte) []byte { return []byte(strings.Repeat("0", 64) + "  " + asset + "\n") }},
		"corrupt archive":   {[]byte("not a tarball"), releaseChecksum(asset)},
		"missing binary":    {missingBinary, releaseChecksum(asset)},
	} {
		t.Run(name, func(t *testing.T) {
			srv := testReleaseServer(t, tc.archive, tc.checksums(tc.archive), nil)
			defer srv.Close()
			defer stub(&releaseFileURLFor, func(tag, name string) string {
				return srv.URL + "/" + tag + "/" + name
			})()
			exe := filepath.Join(t.TempDir(), "coop")
			mustWrite(t, exe, "old binary")
			before, err := os.Stat(exe)
			if err != nil {
				t.Fatal(err)
			}
			if err := installRelease(exe, "v9.9.9"); err == nil {
				t.Fatal("want install failure")
			}
			got, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(exe)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "old binary" || !os.SameFile(before, after) {
				t.Errorf("failed install changed executable: bytes=%q same_inode=%v", got, os.SameFile(before, after))
			}
		})
	}
}

func TestSelfUpdate(t *testing.T) {
	for name, version := range map[string]string{
		"dev build":       "dev",
		"dirty build":     "2.7.2+dirty",
		"malformed build": "banana",
	} {
		t.Run(name+" is a no-op without network", func(t *testing.T) {
			defer stub(&Version, version)()
			defer stub(&githubLatestURL, "http://127.0.0.1:1/must-not-be-called")()
			result, err := selfUpdate()
			if result.Outcome != selfUpdateDev || err != nil {
				t.Fatalf("source build: outcome=%v err=%v", result.Outcome, err)
			}
		})
	}

	t.Run("already current does not fetch artifacts", func(t *testing.T) {
		defer stub(&Version, "2.7.3")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v2.7.3"}`)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		defer stub(&releaseFileURLFor, func(string, string) string {
			t.Error("release artifacts must not be fetched when already current")
			return ""
		})()
		exe := filepath.Join(t.TempDir(), "coop")
		mustWrite(t, exe, "x")
		defer stub(&executablePath, func() (string, error) { return exe, nil })()

		result, err := selfUpdate()
		if result.Outcome != selfUpdateCurrent || err != nil {
			t.Fatalf("current: outcome=%v err=%v", result.Outcome, err)
		}
	})

	t.Run("ahead build is not downgraded", func(t *testing.T) {
		defer stub(&Version, "3.1.0")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v3.0.0"}`)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		defer stub(&releaseFileURLFor, func(string, string) string {
			t.Error("release artifacts must not be fetched for an ahead build")
			return ""
		})()
		exe := filepath.Join(t.TempDir(), "coop")
		mustWrite(t, exe, "ahead")
		defer stub(&executablePath, func() (string, error) { return exe, nil })()

		result, err := selfUpdate()
		if result.Outcome != selfUpdateAhead || err != nil {
			t.Fatalf("ahead: outcome=%v err=%v", result.Outcome, err)
		}
		if got, err := os.ReadFile(exe); err != nil || string(got) != "ahead" {
			t.Fatalf("ahead executable changed: bytes=%q err=%v", got, err)
		}
		if result.Current != "3.1.0" || result.Latest != "3.0.0" {
			t.Errorf("ahead result = %+v, want both versions named", result)
		}
	})

	t.Run("newer release installs verified archive", func(t *testing.T) {
		defer stub(&Version, "2.7.2")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v2.7.3"}`)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		archive := testReleaseArchive(t, "new", "coop")
		asset := fmt.Sprintf("coop_2.7.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
		sums := releaseChecksum(asset)(archive)
		release := testReleaseServer(t, archive, sums, nil)
		defer release.Close()
		defer stub(&releaseFileURLFor, func(tag, name string) string {
			return release.URL + "/" + tag + "/" + name
		})()
		exeDir := t.TempDir()
		exe := filepath.Join(exeDir, "coop")
		mustWrite(t, exe, "old")
		defer stub(&executablePath, func() (string, error) { return exe, nil })()

		result, err := selfUpdate()
		if result.Outcome != selfUpdateInstalled || err != nil {
			t.Fatalf("newer: outcome=%v err=%v", result.Outcome, err)
		}
		if got, err := os.ReadFile(exe); err != nil || string(got) != "new" {
			t.Errorf("verified release was not installed: bytes=%q err=%v", got, err)
		}
		if result.Current != "2.7.2" || result.Latest != "2.7.3" {
			t.Errorf("install result = %+v, want 2.7.2 → 2.7.3", result)
		}
	})

	t.Run("check failure is soft (checkError)", func(t *testing.T) {
		defer stub(&Version, "2.7.2")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		_, err := selfUpdate()
		var ce checkError
		if !errors.As(err, &ce) {
			t.Fatalf("want a checkError (soft), got %v", err)
		}
	})

	t.Run("malformed latest tag is a soft check failure", func(t *testing.T) {
		defer stub(&Version, "2.7.2")()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"latest"}`)
		}))
		defer api.Close()
		defer stub(&githubLatestURL, api.URL)()
		defer stub(&releaseFileURLFor, func(string, string) string {
			t.Error("invalid release tag must not fetch artifacts")
			return ""
		})()
		_, err := selfUpdate()
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

		_, err := selfUpdate()
		if err == nil {
			t.Fatal("want an error for an unwritable install location")
		}
		var ce checkError
		if errors.As(err, &ce) {
			t.Error("unwritable should be a hard error, not a checkError")
		}
		var hard *installFailure
		if !errors.As(err, &hard) || !hard.unchanged {
			t.Errorf("err = %v, want an install failure that left the binary unchanged", err)
		}
		if !strings.Contains(err.Error(), "could not be replaced") {
			t.Errorf("err = %v, want it to say the binary could not be replaced", err)
		}
	})
}

func TestParseUpdateFlags(t *testing.T) {
	cases := []struct {
		name             string
		args             []string
		self, box, check bool
		wantErr          bool
	}{
		{"none", nil, false, false, false, false},
		{"self-only", []string{"--self-only"}, true, false, false, false},
		{"box-only", []string{"--box-only"}, false, true, false, false},
		{"check", []string{"--check"}, false, false, true, false},
		{"both is an error", []string{"--self-only", "--box-only"}, false, false, false, true},
		{"check + self-only is an error", []string{"--check", "--self-only"}, false, false, false, true},
		{"unknown flag", []string{"--wat"}, false, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			self, box, check, err := parseUpdateFlags(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			if self != c.self || box != c.box || check != c.check {
				t.Errorf("self=%v box=%v check=%v, want %v/%v/%v", self, box, check, c.self, c.box, c.check)
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

func testReleaseArchive(t *testing.T, content, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func releaseChecksum(asset string) func([]byte) []byte {
	return func(archive []byte) []byte {
		return []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), asset))
	}
}

func testReleaseServer(t *testing.T, archive, checksums []byte, paths *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if paths != nil {
			*paths = append(*paths, r.URL.Path)
		}
		switch filepath.Base(r.URL.Path) {
		case "checksums.txt":
			_, _ = w.Write(checksums)
		case fmt.Sprintf("coop_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH),
			fmt.Sprintf("coop_2.7.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH):
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
}

// A wrong or hostile asset must fail at the size cap, never be read whole into memory first.
func TestFetchReleaseFileRejectsOversizedAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 17))
	}))
	defer srv.Close()
	defer stub(&releaseFileURLFor, func(tag, name string) string { return srv.URL + "/" + tag + "/" + name })()
	if _, err := fetchReleaseFile("v1.0.0", "big.bin", 16); err == nil || !strings.Contains(err.Error(), "exceeds 16 bytes") {
		t.Fatalf("oversized asset error = %v, want the cap named", err)
	}
	if data, err := fetchReleaseFile("v1.0.0", "fits.bin", 17); err != nil || len(data) != 17 {
		t.Fatalf("asset at the cap = %d bytes, %v; want it accepted", len(data), err)
	}
}
