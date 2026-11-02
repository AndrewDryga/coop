package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func fixtureImageAPI(f dockerFixture) map[string]any {
	e := f.Execution
	if e == nil {
		e = &DockerImageExecution{ID: "sha256:" + strings.Repeat("d", 64)}
	}
	return map[string]any{
		"Id":     e.ID,
		"Config": map[string]any{"Labels": nil, "Entrypoint": e.Entrypoint, "Cmd": e.Cmd, "Shell": e.Shell, "Healthcheck": e.Healthcheck, "Volumes": e.Volumes},
		"RootFS": map[string]any{"Layers": f.Layers},
	}
}

func TestDockerImageReadsFreshTagsWithoutCLI(t *testing.T) {
	rt, file := fixtureDocker(t, dockerFixture{})
	ordinary := fixtureDockerAPI(file)
	wire := fixtureImageAPI(dockerFixture{Layers: []string{"sha256:" + strings.Repeat("1", 64)}})
	endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" {
			ordinary.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/images/registry.example/org/image:tag/json" && r.URL.Path != "/images/"+wire["Id"].(string)+"/json" {
			t.Errorf("wrong image path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(wire)
	}))
	t.Setenv("COOP_DOCKER_FIXTURE_ENDPOINT", endpoint)
	d, err := BindDocker(t.Context(), rt, endpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.binary = "/unavailable-docker"
	c := wire["Config"].(map[string]any)
	c["Env"] = []string{"PASSWORD=inert-private-value", strings.Repeat("x", 128<<10)}
	for _, labels := range []any{nil, map[string]string{}, map[string]string{"definition": "current"}} {
		c["Labels"] = labels
		id, got, err := d.Image(t.Context(), "registry.example/org/image:tag")
		if err != nil || id != wire["Id"] {
			t.Fatalf("fresh image: %s, %v", id, err)
		}
		if labels == nil && got != nil {
			t.Fatal("nil labels changed meaning")
		}
		if want, ok := labels.(map[string]string); ok && !reflect.DeepEqual(got, want) {
			t.Fatalf("labels changed: %v", got)
		}
		wire["Id"] = "sha256:" + strings.Repeat("e", 64)
	}
	delete(c, "Labels")
	if _, labels, err := d.Image(t.Context(), "registry.example/org/image:tag"); err != nil || labels != nil {
		t.Fatalf("omitted labels: %v, %v", labels, err)
	}
	if _, layers, err := d.ImageLayers(t.Context(), "registry.example/org/image:tag"); err != nil || len(layers) != 1 {
		t.Fatalf("layers: %v, %v", layers, err)
	}
	for _, command := range [][]string{nil, {}, {"literal", "$value"}} {
		c["Cmd"] = command
		got, err := d.ImageExecution(t.Context(), wire["Id"].(string))
		if err != nil || !reflect.DeepEqual(got.Cmd, command) {
			t.Fatalf("command semantics: %#v, %v", got.Cmd, err)
		}
		data, _ := json.Marshal(got)
		if strings.Contains(string(data), "inert-private-value") {
			t.Fatal("environment entered observation")
		}
	}
}

func TestDockerImageWireFailuresStayUnknown(t *testing.T) {
	for _, kind := range []string{"malformed", "truncated", "trailing", "missing config", "bad id", "daemon error", "redirect", "raw overflow", "labels overflow", "label count", "command overflow", "canceled body", "replaced after success", "replaced after absence"} {
		t.Run(kind, func(t *testing.T) {
			rt, file := fixtureDocker(t, dockerFixture{})
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
				wire := fixtureImageAPI(dockerFixture{})
				c := wire["Config"].(map[string]any)
				switch kind {
				case "malformed":
					fmt.Fprint(w, "inert-private-value")
					return
				case "truncated":
					fmt.Fprint(w, `{"Config":`)
					return
				case "missing config":
					delete(wire, "Config")
				case "bad id":
					wire["Id"] = "inert-private-value"
				case "daemon error":
					http.Error(w, "inert-private-value", 500)
					return
				case "redirect":
					http.Redirect(w, r, "/info", http.StatusTemporaryRedirect)
					return
				case "raw overflow":
					c["Env"] = []string{strings.Repeat("x", maxDockerImageObservation)}
				case "labels overflow":
					c["Labels"] = map[string]string{"private": strings.Repeat("x", 64<<10)}
				case "label count":
					labels := map[string]string{}
					for i := range 257 {
						labels[fmt.Sprint(i)] = "x"
					}
					c["Labels"] = labels
				case "command overflow":
					c["Cmd"] = []string{strings.Repeat("x", 64<<10)}
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
				if kind == "trailing" {
					fmt.Fprint(w, "inert-private-value")
				}
			}))
			t.Setenv("COOP_DOCKER_FIXTURE_ENDPOINT", endpoint)
			d, err := BindDocker(t.Context(), rt, endpoint, "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if kind == "command overflow" {
				_, err = d.ImageExecution(ctx, "sha256:"+strings.Repeat("d", 64))
			} else {
				_, _, err = d.Image(ctx, "image:tag")
			}
			if err == nil || errors.Is(err, ErrDockerImageMissing) || strings.Contains(err.Error(), "inert-private-value") {
				t.Fatalf("unsafe observation: %v", err)
			}
			if kind == "canceled body" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}
