package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func TestDockerCopyArchiveUsesBoundAPI(t *testing.T) {
	for _, fault := range []string{"none", "oversized", "truncated", "rejected", "redirect", "cancelled", "daemon-replaced", "wrong-owner", "invalid-limit", "invalid-source", "closed"} {
		t.Run(fault, func(t *testing.T) {
			rt, file := fixtureDocker(t, dockerFixture{Container: dockerFixtureContainer()})
			ordinary := fixtureDockerAPI(file)
			var copied atomic.Bool
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			const source = "/observations/a b+?&=%.tar"
			const payload = "private archive bytes"
			endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/info" && copied.Load() && fault == "daemon-replaced" {
					info, err := fixtureDaemonInfo(file)
					if err != nil {
						t.Error(err)
						return
					}
					info.ID = "replacement-daemon"
					_ = json.NewEncoder(w).Encode(info)
					return
				}
				if r.URL.Path != "/containers/"+dockerFixtureRef().ID+"/archive" {
					ordinary.ServeHTTP(w, r)
					return
				}
				copied.Store(true)
				if r.Method != http.MethodGet || len(r.URL.Query()) != 1 || r.URL.Query().Get("path") != source {
					t.Errorf("unexpected archive request: %s %s", r.Method, r.URL)
					http.Error(w, "bad request", 400)
					return
				}
				switch fault {
				case "oversized":
					fmt.Fprint(w, payload+"x")
				case "truncated":
					w.Header().Set("Content-Length", "100")
					fmt.Fprint(w, payload)
				case "rejected":
					http.Error(w, "private-daemon-error", 500)
				case "redirect":
					http.Redirect(w, r, "http://wrong-daemon/", http.StatusTemporaryRedirect)
				case "cancelled":
					cancel()
					<-r.Context().Done()
				default:
					fmt.Fprint(w, payload)
				}
			}))
			d, err := BindDocker(t.Context(), rt, endpoint, "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			d.binary = "/unavailable-docker"
			ref, path, limit := dockerFixtureRef(), source, len(payload)
			switch fault {
			case "wrong-owner":
				ref.Labels = map[string]string{"wrong": "owner"}
			case "invalid-limit":
				limit = 4<<20 + 1
			case "invalid-source":
				path = "relative"
			case "closed":
				d.Close()
			}
			data, err := d.CopyArchive(ctx, ref, path, limit)
			if fault == "none" {
				if err != nil || string(data) != payload {
					t.Fatalf("archive bytes lost: %q %v", data, err)
				}
			} else if err == nil || data != nil {
				t.Fatalf("untrusted archive returned: %q %v", data, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-daemon-error") {
				t.Fatal("daemon error leaked")
			}
			wantCopy := fault != "wrong-owner" && fault != "invalid-limit" && fault != "invalid-source" && fault != "closed"
			if copied.Load() != wantCopy {
				t.Fatalf("archive request = %v, want %v", copied.Load(), wantCopy)
			}
		})
	}
}

// A loaded Docker answers late, not never. The read keeps waiting for its whole bound, which is a
// create's minute and not a control call's 15 seconds. One that outlasts the bound fails as a
// deadline, which the caller turns into a sentence, and the read stops at the bound rather than
// waiting on a daemon that never answers.
func TestDockerCopyArchiveWaitsForASlowDaemonUpToItsBound(t *testing.T) {
	if dockerArchiveTimeout < DockerCreateTimeout {
		t.Fatalf("archive read bound = %s, want at least a create's %s", dockerArchiveTimeout, DockerCreateTimeout)
	}
	const source, payload = "/observations/final.tar", "slow archive bytes"
	for _, tc := range []struct {
		name    string
		bound   time.Duration
		answer  time.Duration // how long the daemon takes to start answering; 0 is never
		wantErr bool
	}{
		{name: "answers late, inside the bound", bound: 5 * time.Second, answer: 150 * time.Millisecond},
		{name: "never answers, so the bound ends the read", bound: 400 * time.Millisecond, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := dockerArchiveTimeout
			dockerArchiveTimeout = tc.bound
			t.Cleanup(func() { dockerArchiveTimeout = old })
			rt, file := fixtureDocker(t, dockerFixture{Container: dockerFixtureContainer()})
			ordinary := fixtureDockerAPI(file)
			endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/containers/"+dockerFixtureRef().ID+"/archive" {
					ordinary.ServeHTTP(w, r)
					return
				}
				if tc.answer == 0 {
					<-r.Context().Done()
					return
				}
				select {
				case <-time.After(tc.answer):
					fmt.Fprint(w, payload)
				case <-r.Context().Done():
				}
			}))
			d, err := BindDocker(t.Context(), rt, endpoint, "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			d.binary = "/unavailable-docker"
			started := time.Now()
			data, err := d.CopyArchive(t.Context(), dockerFixtureRef(), source, len(payload))
			if !tc.wantErr {
				if err != nil || string(data) != payload {
					t.Fatalf("a slow answer inside the bound was lost: %q %v", data, err)
				}
				return
			}
			if err == nil || data != nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a read that outlasts its bound = %q, %v; want a deadline error", data, err)
			}
			if elapsed := time.Since(started); elapsed > tc.bound+3*time.Second {
				t.Fatalf("the read ran %s, long past its %s bound", elapsed, tc.bound)
			}
		})
	}
}
