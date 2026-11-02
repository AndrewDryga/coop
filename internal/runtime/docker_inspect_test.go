package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureDockerAPI(file string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" {
			info, err := fixtureDaemonInfo(file)
			if err != nil {
				http.Error(w, "unavailable", 500)
				return
			}
			_ = json.NewEncoder(w).Encode(info)
			return
		}
		var fixture dockerFixture
		data, err := os.ReadFile(file)
		if err != nil || json.Unmarshal(data, &fixture) != nil {
			http.Error(w, "unavailable", 500)
			return
		}
		if r.Method == http.MethodDelete && fixture.Container != nil && r.URL.Path == "/containers/"+fixture.Container.ID && r.URL.RawQuery == "force=true" {
			fixture.Container = nil
			fixture.Mutations++
			data, _ := json.Marshal(fixture)
			if os.WriteFile(file, data, 0600) != nil {
				http.Error(w, "fixture write failed", 500)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && fixture.Container != nil && r.URL.Path == "/containers/"+fixture.Container.ID+"/start" && fixture.Mode != "no-start" {
			fixture.Container.State = DockerContainerState{Status: "running", Running: true, StartedAt: time.Now().UTC()}
			fixture.Mutations++
			data, _ := json.Marshal(fixture)
			if os.WriteFile(file, data, 0600) != nil {
				http.Error(w, "fixture write failed", 500)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/volumes/") && fixture.Mode == "shared-volume" {
			if fixture.SharedVolume == nil || strings.TrimPrefix(r.URL.Path, "/volumes/") != fixture.SharedVolume.Name {
				http.NotFound(w, r)
				return
			}
			if fixture.AfterVolumeID != "" {
				fixture.ID = fixture.AfterVolumeID
				data, _ := json.Marshal(fixture)
				if os.WriteFile(file+".volume", data, 0600) != nil || os.Rename(file+".volume", file) != nil {
					http.Error(w, "fixture write failed", 500)
					return
				}
			}
			_ = json.NewEncoder(w).Encode(fixture.SharedVolume)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json") {
			if fixture.Mode == "execution-malformed" {
				fmt.Fprint(w, "invalid JSON")
			} else {
				_ = json.NewEncoder(w).Encode(fixtureImageAPI(fixture))
			}
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/containers/") || !strings.HasSuffix(r.URL.Path, "/json") {
			http.NotFound(w, r)
			return
		}
		switch fixture.Mode {
		case "overflow":
			_, _ = w.Write([]byte(strings.Repeat("x", maxDockerContainerObservation+1)))
			return
		case "inspect-fail", "list-fail":
			http.Error(w, "unavailable", 500)
			return
		}
		c := fixture.Container
		if c == nil {
			http.NotFound(w, r)
			return
		}
		if fixture.Mode == "settling" && !fixture.SettleAt.IsZero() && time.Now().After(fixture.SettleAt) {
			c.State = DockerContainerState{Status: "exited", StartedAt: c.State.StartedAt, FinishedAt: time.Now().UTC(), ExitCode: settlingExitCode}
		}
		_ = json.NewEncoder(w).Encode(fixtureContainerAPI(c))
	})
}

// Docker's wire shape is nested, unlike the old CLI's projected template.
func fixtureContainerAPI(c *DockerContainer) map[string]any {
	return map[string]any{
		"Id": c.ID, "Name": c.Name, "Image": c.Image, "RestartCount": c.RestartCount, "State": c.State, "Mounts": c.Mounts,
		"Config":     map[string]any{"User": c.User, "Labels": c.Labels, "Tty": c.Tty, "OpenStdin": c.OpenStdin, "Entrypoint": c.Entrypoint, "Cmd": c.Cmd, "Shell": c.Shell, "Healthcheck": c.Healthcheck},
		"HostConfig": map[string]any{"NetworkMode": c.NetworkMode, "PidMode": c.PidMode, "IpcMode": c.IpcMode, "UsernsMode": c.UsernsMode, "CgroupnsMode": c.CgroupnsMode, "CgroupParent": c.CgroupParent, "CapAdd": c.CapAdd, "CapDrop": c.CapDrop, "SecurityOpt": c.SecurityOpt, "Privileged": c.Privileged, "AutoRemove": c.AutoRemove, "ReadonlyRootfs": c.ReadonlyRootfs, "RestartPolicy": map[string]string{"Name": c.RestartPolicy}, "Tmpfs": c.Tmpfs},
	}
}

func TestDockerInspectUsesBoundAPI(t *testing.T) {
	c := dockerFixtureContainer()
	c.CgroupParent = "/coop-fs-test"
	c.Entrypoint = []string{"/guard"}
	c.Cmd = []string{"guard", "/manifest"}
	c.Tmpfs = map[string]string{"/tmp": "rw"}
	rt, _ := fixtureDocker(t, dockerFixture{Container: c})
	d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Inspection must work without a CLI process. Mutations still require it.
	d.binary = "/unavailable-docker"
	got, present, err := d.InspectContainer(t.Context(), dockerFixtureRef())
	if err != nil || !present || got.CgroupParent != c.CgroupParent || got.Entrypoint[0] != "/guard" || got.Tmpfs["/tmp"] != "rw" {
		t.Fatalf("lost bound inspection: %+v %v", got, err)
	}
}

func TestDockerInspectWireFailuresStayUnknown(t *testing.T) {
	for _, kind := range []string{"malformed", "truncated", "missing config", "missing host", "missing state", "daemon error", "redirect", "canceled body", "replaced after success", "replaced after absence"} {
		t.Run(kind, func(t *testing.T) {
			rt, file := fixtureDocker(t, dockerFixture{Container: dockerFixtureContainer()})
			ordinary := fixtureDockerAPI(file)
			var replaced atomic.Bool
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/info" {
					if replaced.Load() {
						info, err := fixtureDaemonInfo(file)
						if err != nil {
							http.Error(w, "fixture failure", 500)
							return
						}
						info.ID = "replacement-daemon"
						_ = json.NewEncoder(w).Encode(info)
					} else {
						ordinary.ServeHTTP(w, r)
					}
					return
				}
				wire := fixtureContainerAPI(dockerFixtureContainer())
				switch kind {
				case "malformed":
					fmt.Fprint(w, "inert-private-value")
					return
				case "truncated":
					fmt.Fprint(w, `{"Config":`)
					return
				case "missing config":
					delete(wire, "Config")
				case "missing host":
					delete(wire, "HostConfig")
				case "missing state":
					delete(wire, "State")
				case "daemon error":
					http.Error(w, "inert-private-value", 500)
					return
				case "redirect":
					http.Redirect(w, r, "/info", http.StatusTemporaryRedirect)
					return
				case "canceled body":
					w.Header().Set("Content-Length", "4096")
					fmt.Fprint(w, "{")
					w.(http.Flusher).Flush()
					cancel()
					<-r.Context().Done()
					return
				case "replaced after success":
					replaced.Store(true)
				case "replaced after absence":
					replaced.Store(true)
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(wire)
			}))
			t.Setenv("COOP_DOCKER_FIXTURE_ENDPOINT", endpoint)
			d, err := BindDocker(t.Context(), rt, endpoint, "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			_, present, err := d.InspectContainer(ctx, dockerFixtureRef())
			if err == nil || present || strings.Contains(err.Error(), "inert-private-value") {
				t.Fatalf("unsafe observation: present=%t, err=%v", present, err)
			}
			if kind == "canceled body" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}

func TestDockerInspectRetainsCompleteSecurityState(t *testing.T) {
	c := dockerFixtureContainer()
	c.User, c.NetworkMode, c.PidMode, c.IpcMode, c.UsernsMode, c.CgroupnsMode, c.CgroupParent = "123:456", "host", "host", "host", "host", "host", "/parent"
	c.Entrypoint, c.Cmd, c.Shell = []string{"entry"}, []string{"arg"}, []string{"shell"}
	c.CapAdd, c.CapDrop, c.SecurityOpt = []string{"SYS_ADMIN"}, []string{"ALL"}, []string{"no-new-privileges"}
	c.Privileged, c.AutoRemove, c.ReadonlyRootfs, c.Tty, c.OpenStdin = true, true, true, true, true
	c.Healthcheck = &DockerHealthcheck{Test: []string{"CMD", "health"}}
	c.RestartCount, c.RestartPolicy = 3, "always"
	c.State = DockerContainerState{Status: "paused", Running: true, Paused: true, OOMKilled: true, ExitCode: 42, StartedAt: time.Unix(100, 0).UTC(), FinishedAt: time.Unix(200, 0).UTC()}
	c.Tmpfs = map[string]string{"/tmp": "rw"}
	c.Mounts = []DockerMount{{Type: "bind", Source: "/host", Destination: "/guest", RW: true}, {Type: "volume", Name: "volume", Source: "/vol", Destination: "/data"}}
	rt, file := fixtureDocker(t, dockerFixture{Container: c})
	ordinary := fixtureDockerAPI(file)
	endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" {
			ordinary.ServeHTTP(w, r)
			return
		}
		wire := fixtureContainerAPI(c)
		wire["Config"].(map[string]any)["Env"] = []string{"PASSWORD=inert-private-value"}
		_ = json.NewEncoder(w).Encode(wire)
	}))
	t.Setenv("COOP_DOCKER_FIXTURE_ENDPOINT", endpoint)
	d, err := BindDocker(t.Context(), rt, endpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, present, err := d.InspectContainer(t.Context(), dockerFixtureRef())
	c.Name = strings.TrimPrefix(c.Name, "/")
	if err != nil || !present || !reflect.DeepEqual(got, *c) {
		t.Fatalf("lost security state: %+v, %v", got, err)
	}
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), "inert-private-value") {
		t.Fatal("environment entered observation")
	}
}

func TestDockerInspectAcceptsFullWireMountPlan(t *testing.T) {
	c := dockerFixtureContainer()
	var binds []string
	for i := range maxDockerMounts {
		source := "/host/" + strings.Repeat("directory/", 20) + fmt.Sprint(i)
		target := "/workspace/" + strings.Repeat("directory/", 20) + fmt.Sprint(i)
		c.Mounts = append(c.Mounts, DockerMount{Type: "bind", Source: source, Destination: target})
		binds = append(binds, source+":"+target+":ro")
	}
	rt, file := fixtureDocker(t, dockerFixture{Container: c})
	ordinary := fixtureDockerAPI(file)
	wire := fixtureContainerAPI(c)
	wire["HostConfig"].(map[string]any)["Binds"] = binds
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 4<<20 || len(data) >= maxDockerContainerObservation {
		t.Fatalf("fixture wire size %d does not exercise duplicate mount fields", len(data))
	}
	endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" {
			ordinary.ServeHTTP(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Setenv("COOP_DOCKER_FIXTURE_ENDPOINT", endpoint)
	d, err := BindDocker(t.Context(), rt, endpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, present, err := d.InspectContainer(t.Context(), dockerFixtureRef())
	if err != nil || !present || len(got.Mounts) != maxDockerMounts {
		t.Fatalf("valid full mount plan refused: %d, %v", len(got.Mounts), err)
	}
}
