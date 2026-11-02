package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func TestDockerNetworksReadsFreshTopologyWithoutCLI(t *testing.T) {
	rt, file := fixtureDocker(t, dockerFixture{})
	ordinary := fixtureDockerAPI(file)
	subnet := "172.18.7.1/16"
	endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" {
			ordinary.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/networks" || r.URL.RawQuery != "" {
			t.Errorf("unexpected network inventory request: %s %s", r.Method, r.URL)
		}
		_ = json.NewEncoder(w).Encode([]any{
			map[string]any{"Id": strings.Repeat("1", 64), "Name": "bridge", "Scope": "local", "Internal": true, "IPAM": map[string]any{"Config": []any{
				map[string]string{"Subnet": subnet, "Gateway": "172.18.0.1"},
				map[string]string{"Subnet": "fd00::/64", "Gateway": "fd00::1"},
			}}},
			map[string]any{"Id": strings.Repeat("2", 64), "Name": "host", "Scope": "local", "IPAM": map[string]any{"Config": nil}},
			map[string]any{"Id": "fsf1dmx3i9q75an49z36jycxd", "Name": "overlay", "Scope": "global", "IPAM": map[string]any{"Config": []any{
				map[string]string{"Subnet": "10.0.0.0/24", "Gateway": "10.0.0.1"},
			}}},
		})
	}))
	t.Setenv("COOP_DOCKER_FIXTURE_ENDPOINT", endpoint)
	d, err := BindDocker(t.Context(), rt, endpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.binary = "/unavailable-docker"
	for _, value := range []string{"172.18.7.1/16", "172.19.1.1/24"} {
		subnet = value
		got, err := d.Networks(t.Context())
		want := []DockerNetwork{
			{Name: "bridge", Internal: true, Subnets: []netip.Prefix{netip.MustParsePrefix(value).Masked()}, Gateways: []netip.Addr{netip.MustParseAddr("172.18.0.1")}},
			{Name: "host"},
			{Name: "overlay", Subnets: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}, Gateways: []netip.Addr{netip.MustParseAddr("10.0.0.1")}},
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("fresh topology: got %+v, %v; want %+v", got, err, want)
		}
	}
}

func TestDockerNetworksRejectsIncompleteObservation(t *testing.T) {
	for _, kind := range []string{"malformed", "truncated", "trailing", "empty", "missing identity", "missing name", "missing ipam", "missing scope", "unknown scope", "duplicate", "too many", "wire overflow", "projection overflow", "daemon error", "redirect", "canceled body", "replaced after success", "replaced after error"} {
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
							t.Error(err)
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
				value := map[string]any{"Id": strings.Repeat("1", 64), "Name": "bridge", "Scope": "local", "IPAM": map[string]any{"Config": []any{map[string]string{"Subnet": "172.18.0.0/16", "Gateway": "172.18.0.1"}}}}
				values := []any{value}
				switch kind {
				case "malformed":
					fmt.Fprint(w, "inert-private-value")
					return
				case "truncated":
					fmt.Fprint(w, `[{"Id":`)
					return
				case "empty":
					values = nil
				case "missing identity":
					delete(value, "Id")
				case "missing name":
					delete(value, "Name")
				case "missing ipam":
					delete(value, "IPAM")
				case "missing scope":
					delete(value, "Scope")
				case "unknown scope":
					value["Scope"] = "unknown"
				case "duplicate":
					values = append(values, value)
				case "too many":
					for i := 1; i <= maxDockerNetworks; i++ {
						values = append(values, value)
					}
				case "wire overflow":
					value["Labels"] = strings.Repeat("x", 4<<20)
				case "projection overflow":
					value["Name"] = strings.Repeat("x", 1<<20)
				case "daemon error":
					w.WriteHeader(http.StatusInternalServerError)
				case "redirect":
					http.Redirect(w, r, "/info", http.StatusTemporaryRedirect)
					return
				case "canceled body":
					w.Header().Set("Content-Length", "4096")
					fmt.Fprint(w, "[")
					w.(http.Flusher).Flush()
					cancel()
					<-r.Context().Done()
					return
				case "replaced after success":
					replaced.Store(true)
				case "replaced after error":
					replaced.Store(true)
					w.WriteHeader(http.StatusInternalServerError)
				}
				value["UnusedPrivateValue"] = "inert-private-value"
				_ = json.NewEncoder(w).Encode(values)
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
			got, err := d.Networks(ctx)
			if err == nil || got != nil || strings.Contains(err.Error(), "inert-private-value") {
				t.Fatalf("unusable response returned topology or private body: %+v %v", got, err)
			}
			if strings.HasPrefix(kind, "replaced") && !strings.Contains(err.Error(), "Docker daemon identity changed") {
				t.Fatalf("daemon replacement lost: %v", err)
			}
		})
	}
}

func TestDockerNetworksBoundsAndCancellation(t *testing.T) {
	rt, file := fixtureDocker(t, dockerFixture{})
	ordinary := fixtureDockerAPI(file)
	var observations atomic.Int32
	endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" {
			ordinary.ServeHTTP(w, r)
			return
		}
		observations.Add(1)
		values := make([]any, maxDockerNetworks)
		for i := range values {
			values[i] = map[string]any{"Id": fmt.Sprintf("%064x", i+1), "Name": fmt.Sprint("network-", i), "Scope": "local", "IPAM": map[string]any{"Config": nil}}
		}
		_ = json.NewEncoder(w).Encode(values)
	}))
	t.Setenv("COOP_DOCKER_FIXTURE_ENDPOINT", endpoint)
	d, err := BindDocker(t.Context(), rt, endpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.Networks(t.Context())
	if err != nil || len(got) != maxDockerNetworks {
		t.Fatalf("maximum valid inventory: %d %v", len(got), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, ctx := range []context.Context{nil, ctx} {
		if got, err := d.Networks(ctx); err == nil || got != nil {
			t.Fatalf("invalid operation accepted: %+v %v", got, err)
		}
	}
	if observations.Load() != 1 {
		t.Fatal("canceled or context-free operation reached networks endpoint")
	}
}

func TestDockerNetworksInspectsSwarmLocalAllocations(t *testing.T) {
	for _, kind := range []string{"allocated", "missing", "wrong identity", "wrong name", "missing ipam", "malformed", "wire overflow", "aggregate overflow", "canceled", "replaced"} {
		t.Run(kind, func(t *testing.T) {
			rt, file := fixtureDocker(t, dockerFixture{})
			ordinary := fixtureDockerAPI(file)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var replaced atomic.Bool
			var inspections atomic.Int32
			const id = "fsf1dmx3i9q75an49z36jycxd"
			endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/info" {
					if replaced.Load() {
						info, err := fixtureDaemonInfo(file)
						if err != nil {
							t.Error(err)
							return
						}
						info.ID = "replacement-daemon"
						_ = json.NewEncoder(w).Encode(info)
					} else {
						ordinary.ServeHTTP(w, r)
					}
					return
				}
				value := map[string]any{"Id": id, "Name": "swarm-bridge", "Scope": "swarm", "IPAM": map[string]any{"Config": nil}}
				if r.URL.Path == "/networks" {
					if kind == "aggregate overflow" {
						value["Labels"] = strings.Repeat("x", 2<<20)
					}
					_ = json.NewEncoder(w).Encode([]any{map[string]any{"Id": strings.Repeat("1", 64), "Name": "host", "Scope": "local", "IPAM": map[string]any{"Config": nil}}, value})
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/networks/"+id || r.URL.RawQuery != "" {
					t.Errorf("unexpected Swarm lookup: %s %s", r.Method, r.URL)
				}
				inspections.Add(1)
				value["IPAM"] = map[string]any{"Config": []any{map[string]string{"Subnet": "172.31.0.0/16", "Gateway": "172.31.0.1"}}}
				switch kind {
				case "missing":
					w.WriteHeader(http.StatusNotFound)
				case "wrong identity":
					value["Id"] = "another-network"
				case "wrong name":
					value["Name"] = "another-network"
				case "missing ipam":
					delete(value, "IPAM")
				case "malformed":
					fmt.Fprint(w, "inert-private-value")
					return
				case "wire overflow":
					value["Labels"] = strings.Repeat("x", 4<<20)
				case "aggregate overflow":
					value["Labels"] = strings.Repeat("x", 3<<20)
				case "canceled":
					w.Header().Set("Content-Length", "4096")
					fmt.Fprint(w, "{")
					w.(http.Flusher).Flush()
					cancel()
					<-r.Context().Done()
					return
				case "replaced":
					replaced.Store(true)
				}
				value["UnusedPrivateValue"] = "inert-private-value"
				_ = json.NewEncoder(w).Encode(value)
			}))
			t.Setenv("COOP_DOCKER_FIXTURE_ENDPOINT", endpoint)
			d, err := BindDocker(t.Context(), rt, endpoint, "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			d.binary = "/unavailable-docker"
			got, err := d.Networks(ctx)
			if kind == "allocated" {
				want := []DockerNetwork{{Name: "host"}, {Name: "swarm-bridge", Subnets: []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.31.0.1")}}}
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("local allocation lost: %+v %v", got, err)
				}
			} else if err == nil || got != nil || strings.Contains(err.Error(), "inert-private-value") {
				t.Fatalf("unusable inspection returned topology or private body: %+v %v", got, err)
			}
			if kind == "replaced" && !strings.Contains(err.Error(), "Docker daemon identity changed") {
				t.Fatalf("daemon replacement lost: %v", err)
			}
			if inspections.Load() != 1 {
				t.Fatalf("Swarm inspection count: %d", inspections.Load())
			}
		})
	}
}
