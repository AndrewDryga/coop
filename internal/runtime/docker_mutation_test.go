package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func TestDockerContainerMutationsUseBoundAPI(t *testing.T) {
	rt, _ := fixtureDocker(t, dockerFixture{Container: dockerFixtureContainer()})
	d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.binary = "/unavailable-docker"
	ref := dockerFixtureRef()
	if err := d.StartContainer(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if err := d.StartContainer(t.Context(), ref); err == nil {
		t.Fatal("restarted container")
	}
	if err := d.StopContainer(t.Context(), ref, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveContainer(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveContainer(t.Context(), ref); err != nil {
		t.Fatal("absence not idempotent", err)
	}
}

func TestDockerContainerMutationUnknownOutcomes(t *testing.T) {
	for _, operation := range []string{"start", "stop", "remove"} {
		for _, fault := range []string{"rejected", "redirect", "oversized", "truncated", "cancelled", "unchanged", "daemon-replaced", "lost-reply", "lost-stop-reply"} {
			if fault == "lost-stop-reply" && operation != "stop" {
				continue
			}
			t.Run(operation+"/"+fault, func(t *testing.T) {
				container := dockerFixtureContainer()
				if operation == "stop" {
					container.State = DockerContainerState{Status: "running", Running: true}
				}
				rt, file := fixtureDocker(t, dockerFixture{Container: container})
				ordinary := fixtureDockerAPI(file)
				var mutated atomic.Bool
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/info" && fault == "daemon-replaced" && mutated.Load() {
						info, err := fixtureDaemonInfo(file)
						if err != nil {
							t.Error(err)
							http.Error(w, "fixture", 500)
							return
						}
						info.ID = "other-daemon"
						_ = json.NewEncoder(w).Encode(info)
						return
					}
					if r.Method == http.MethodGet {
						ordinary.ServeHTTP(w, r)
						return
					}
					mutated.Store(true)
					switch fault {
					case "rejected":
						http.Error(w, "private-daemon-error", 500)
					case "redirect":
						http.Redirect(w, r, "http://wrong-daemon/", http.StatusTemporaryRedirect)
					case "oversized":
						fmt.Fprint(w, strings.Repeat("x", 1025))
					case "truncated":
						w.Header().Set("Content-Length", "100")
						w.WriteHeader(http.StatusOK)
						fmt.Fprint(w, "short")
					case "cancelled":
						cancel()
						<-r.Context().Done()
					case "unchanged":
						w.WriteHeader(http.StatusNoContent)
					case "daemon-replaced":
						ordinary.ServeHTTP(w, r)
					case "lost-reply", "lost-stop-reply":
						data, err := os.ReadFile(file)
						if err != nil {
							t.Error(err)
							return
						}
						var fixture dockerFixture
						if err = json.Unmarshal(data, &fixture); err != nil {
							t.Error(err)
							return
						}
						if fault == "lost-stop-reply" {
							fixture.Container.State = DockerContainerState{Status: "exited"}
						} else {
							fixture.Container = nil
						}
						data, _ = json.Marshal(fixture)
						if err = os.WriteFile(file, data, 0600); err != nil {
							t.Error(err)
							return
						}
						http.Error(w, "private-daemon-error", 500)
					}
				}))
				d, err := BindDocker(t.Context(), rt, endpoint, "")
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				d.binary = "/unavailable-docker"
				if operation == "start" {
					err = d.StartContainer(ctx, dockerFixtureRef())
				} else if operation == "stop" {
					err = d.StopContainer(ctx, dockerFixtureRef(), 0)
				} else {
					err = d.RemoveContainer(ctx, dockerFixtureRef())
				}
				wantSuccess := operation != "start" && (fault == "lost-reply" || fault == "lost-stop-reply")
				if (err == nil) != wantSuccess {
					t.Fatalf("success=%v error=%v", wantSuccess, err)
				}
				if err != nil && strings.Contains(err.Error(), "private-daemon-error") {
					t.Fatal("daemon response leaked")
				}
				if !mutated.Load() {
					t.Fatal("mutation was not exercised")
				}
			})
		}
	}
}

func TestDockerContainerMutationRequiresCustody(t *testing.T) {
	rt, _ := fixtureDocker(t, dockerFixture{Container: dockerFixtureContainer()})
	d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ref := dockerFixtureRef()
	ref.Labels = map[string]string{"wrong": "owner"}
	if err := d.StartContainer(t.Context(), ref); err == nil {
		t.Fatal("started foreign container")
	}
	if err := d.StopContainer(t.Context(), ref, 0); err == nil {
		t.Fatal("stopped foreign container")
	}
	if err := d.RemoveContainer(t.Context(), ref); err == nil {
		t.Fatal("removed foreign container")
	}
	value, present, err := d.InspectContainer(t.Context(), dockerFixtureRef())
	if err != nil || !present || value.State.Status != "created" {
		t.Fatal("foreign container changed", value, err)
	}
}
