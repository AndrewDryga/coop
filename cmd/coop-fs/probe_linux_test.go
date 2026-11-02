//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
)

func TestProbeShareRequiresPublicContent(t *testing.T) {
	for _, body := range []string{fsfilter.ProbeContent, "incorrect", ""} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			public := filepath.Join(dir, "public")
			if body != "" {
				if err := os.WriteFile(public, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(fsfilter.ShareProbe{Readable: []string{public}})
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "probe.json")
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := probeShare(file, true); (err == nil) != (body == fsfilter.ProbeContent) {
				t.Fatalf("public control %q: %v", body, err)
			}
		})
	}
}

func TestProbeDeniedRequiresPermissionAfterRenameVisibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		results []error
		wantErr bool
		calls   int
	}{
		{"permission", []error{syscall.EPERM}, false, 1},
		{"access", []error{syscall.EACCES}, false, 1},
		{"missing then permission", []error{syscall.ENOENT, syscall.EPERM}, false, 2},
		{"missing then access", []error{syscall.ENOENT, syscall.ENOENT, syscall.EACCES}, false, 3},
		{"readable immediately", []error{nil, syscall.EPERM}, true, 1},
		{"missing then readable", []error{syscall.ENOENT, nil, syscall.EPERM}, true, 2},
		{"missing then IO error", []error{syscall.ENOENT, syscall.EIO, syscall.EPERM}, true, 2},
		{"missing then wrong type", []error{syscall.ENOENT, syscall.ENOTDIR, syscall.EPERM}, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				start := time.Now()
				missing, err := probeDenied([]string{"warm-alias", "other-alias"}, func(path string) error {
					if path == "other-alias" {
						if calls != tc.calls || tc.wantErr {
							t.Fatal("another alias refreshed before decisive denial")
						}
						return syscall.EPERM
					}
					if calls >= len(tc.results) {
						t.Fatal("retried an unexpected outcome")
					}
					result := tc.results[calls]
					calls++
					if result != nil {
						return &os.PathError{Op: "open", Path: path, Err: result}
					}
					return nil
				})
				if (err != nil) != tc.wantErr || calls != tc.calls {
					t.Fatalf("calls=%d missing=%d error=%v", calls, missing, err)
				}
				wantMissing := 0
				for _, result := range tc.results[:tc.calls] {
					if result == syscall.ENOENT {
						wantMissing++
					}
				}
				if missing != wantMissing || time.Since(start) != time.Duration(wantMissing)*10*time.Millisecond {
					t.Fatalf("missing=%d, elapsed=%s", missing, time.Since(start))
				}
			})
		})
	}
}

func TestProbeDeniedSharesOneVisibilityDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var seen []string
		missing, err := probeDenied([]string{"warm-alias", "other-alias", "unreached"}, func(path string) error {
			if len(seen) == 0 || seen[len(seen)-1] != path {
				seen = append(seen, path)
			}
			if path == "warm-alias" && time.Since(start) >= 1500*time.Millisecond {
				return syscall.EPERM
			}
			return syscall.ENOENT
		})
		if err == nil || missing < 2 || time.Since(start) != 2*time.Second || !reflect.DeepEqual(seen, []string{"warm-alias", "other-alias"}) {
			t.Fatalf("absence accepted or renewed its budget: seen=%v missing=%d elapsed=%s error=%v", seen, missing, time.Since(start), err)
		}
	})
}
