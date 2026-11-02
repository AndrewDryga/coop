package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func TestDockerVolumeExposureReadsFreshDefinitionsWithoutCLI(t *testing.T) {
	rt, file := fixtureDocker(t, dockerFixture{})
	ordinary := fixtureDockerAPI(file)
	var payload atomic.Value
	var reads atomic.Int32
	endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" {
			ordinary.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/volumes/coop-cache" {
			t.Errorf("unexpected volume operation: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected operation", 500)
			return
		}
		reads.Add(1)
		if data := payload.Load().(string); data != "" {
			fmt.Fprint(w, data)
		} else {
			http.NotFound(w, r)
		}
	}))
	d, err := BindDocker(t.Context(), rt, endpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.binary = "/unavailable-docker"
	t.Setenv("DOCKER_HOST", "unix:///unrelated.sock")
	t.Setenv("DOCKER_CONTEXT", "unrelated")
	definition := volumeDefinition{Name: "coop-cache", Driver: "local", Scope: "local", Mountpoint: t.TempDir()}
	for _, options := range []map[string]string{nil, {"type": "none", "device": t.TempDir(), "o": "bind"}} {
		definition.Options = options
		data, _ := json.Marshal(definition)
		var wire map[string]any
		_ = json.Unmarshal(data, &wire)
		wire["Labels"] = map[string]string{"unselected": strings.Repeat("x", 128<<10)}
		data, _ = json.Marshal(wire)
		payload.Store(string(data))
		exposure, err := d.ExistingNamedVolumeExposure(t.Context(), []string{"coop-cache", "coop-cache"})
		if err != nil || len(exposure.Sources) == 0 || exposure.Sources[0] != definition.Mountpoint {
			t.Fatalf("fresh bound volume: %+v, %v", exposure, err)
		}
		if options == nil {
			if exposure.PlainLocalVolumes[definition.Name].Mountpoint != definition.Mountpoint || len(exposure.BindSources) != 0 {
				t.Fatalf("lost plain volume provenance: %+v", exposure)
			}
		} else if !slices.Equal(exposure.BindSources, []string{options["device"]}) || len(exposure.PlainLocalVolumes) != 0 {
			t.Fatalf("lost changed bind exposure: %+v", exposure)
		}
	}
	payload.Store("")
	if exposure, err := d.ExistingNamedVolumeExposure(t.Context(), []string{"coop-cache"}); err != nil || !reflect.DeepEqual(exposure, VolumeExposure{}) {
		t.Fatalf("confirmed absent volume: %+v, %v", exposure, err)
	}
	if reads.Load() != 3 {
		t.Fatalf("expected one fresh inspection per distinct volume: %d", reads.Load())
	}
	for _, names := range [][]string{{"../other"}, make([]string, 65)} {
		if _, err := d.ExistingNamedVolumeExposure(t.Context(), names); err == nil {
			t.Fatalf("invalid selection accepted: %q", names)
		}
	}
	if reads.Load() != 3 {
		t.Fatal("invalid selection reached volume inspection")
	}
}

func TestDockerVolumeExposureWireFailuresStayUnknown(t *testing.T) {
	for _, kind := range []string{"malformed", "truncated", "trailing", "null", "wrong name", "unsupported driver", "missing mountpoint", "options count", "projection overflow", "wire overflow", "truncated absence", "oversized absence", "daemon error", "redirect", "canceled body", "replaced after success", "replaced after absence", "later volume fails"} {
		t.Run(kind, func(t *testing.T) {
			rt, file := fixtureDocker(t, dockerFixture{})
			ordinary := fixtureDockerAPI(file)
			var replaced atomic.Bool
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/info" {
					if replaced.Load() {
						info, _ := fixtureDaemonInfo(file)
						info.ID = "replacement-daemon"
						_ = json.NewEncoder(w).Encode(info)
					} else {
						ordinary.ServeHTTP(w, r)
					}
					return
				}
				wire := map[string]any{"Name": "coop-cache", "Driver": "local", "Scope": "local", "Mountpoint": "/daemon/volumes/coop-cache/_data", "Options": nil}
				switch kind {
				case "malformed":
					fmt.Fprint(w, "inert-private-value")
					return
				case "truncated":
					fmt.Fprint(w, `{"Name":`)
					return
				case "null":
					fmt.Fprint(w, "null")
					return
				case "wrong name":
					wire["Name"] = "other"
				case "unsupported driver":
					wire["Driver"] = "custom"
				case "missing mountpoint":
					delete(wire, "Mountpoint")
				case "options count":
					options := map[string]string{}
					for i := range 33 {
						options[fmt.Sprint(i)] = "x"
					}
					wire["Options"] = options
				case "projection overflow":
					wire["CreatedAt"] = strings.Repeat("x", 64<<10)
				case "wire overflow":
					wire["Labels"] = map[string]string{"unused": strings.Repeat("x", 1<<20)}
				case "truncated absence":
					w.Header().Set("Content-Length", "4096")
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, "{")
					return
				case "oversized absence":
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, strings.Repeat("x", (1<<20)+1))
					return
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
				case "later volume fails":
					if r.URL.Path == "/volumes/second" {
						http.Error(w, "inert-private-value", 500)
						return
					}
				}
				_ = json.NewEncoder(w).Encode(wire)
				if kind == "trailing" {
					fmt.Fprint(w, "inert-private-value")
				}
			}))
			d, err := BindDocker(t.Context(), rt, endpoint, "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			names := []string{"coop-cache"}
			if kind == "later volume fails" {
				names = append(names, "second")
			}
			exposure, err := d.ExistingNamedVolumeExposure(ctx, names)
			if err == nil || !reflect.DeepEqual(exposure, VolumeExposure{}) || strings.Contains(err.Error(), "inert-private-value") {
				t.Fatalf("unsafe volume observation: %+v, %v", exposure, err)
			}
			if kind == "canceled body" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}
