package box

import (
	"slices"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestFilesystemAgentImageAdmission(t *testing.T) {
	for _, test := range []struct {
		name    string
		health  []string
		volume  string
		allowed bool
	}{
		{"plain", nil, "", true}, {"disabled", []string{"NONE"}, "", true},
		{"exec health", []string{"CMD", "true"}, "", false}, {"shell health", []string{"CMD-SHELL", "true"}, "", false},
		{"root volume", nil, "/", false}, {"control volume", nil, "/.coop-fs", false},
		{"nested control", nil, "/.coop-fs/guard", false}, {"other volume", nil, "/data", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			image := runtime.DockerImageExecution{}
			if test.health != nil {
				image.Healthcheck = &runtime.DockerHealthcheck{Test: test.health}
			}
			if test.volume != "" {
				image.Volumes = map[string]struct{}{test.volume: {}}
			}
			if (filesystemImageSupported(image) == nil) != test.allowed {
				t.Fatal("incorrect admission")
			}
		})
	}
}

func TestFilesystemServiceExecutionPreservesComposeSemantics(t *testing.T) {
	image := runtime.DockerImageExecution{Entrypoint: []string{"image-entry"}, Cmd: []string{"image$$arg"}, Shell: []string{"custom-shell", "-c"}, Healthcheck: &runtime.DockerHealthcheck{Test: []string{"CMD-SHELL", "image$$health"}, Interval: 5 * time.Second, Retries: 2}}
	for _, test := range []struct {
		name            string
		service         map[string]any
		command, health []string
	}{
		{"image defaults", map[string]any{}, []string{"image-entry", "image$$arg"}, []string{"custom-shell", "-c", "image$$health"}},
		{"command dollars", map[string]any{"command": []any{"echo", "$$VALUE", "cost$$5"}}, []string{"image-entry", "echo", "$VALUE", "cost$5"}, []string{"custom-shell", "-c", "image$$health"}},
		{"explicit entry suppresses cmd", map[string]any{"entrypoint": []any{"custom$$entry"}}, []string{"custom$entry"}, []string{"custom-shell", "-c", "image$$health"}},
		{"cleared entry", map[string]any{"entrypoint": []any{}, "command": []any{"program"}}, []string{"program"}, []string{"custom-shell", "-c", "image$$health"}},
		{"compose shell health", map[string]any{"healthcheck": map[string]any{"test": []any{"CMD-SHELL", "echo $$VALUE"}}}, []string{"image-entry", "image$$arg"}, []string{"custom-shell", "-c", "echo $VALUE"}},
		{"compose exec health", map[string]any{"healthcheck": map[string]any{"test": []any{"CMD", "check", "$$arg"}}}, []string{"image-entry", "image$$arg"}, []string{"check", "$arg"}},
		{"disabled health", map[string]any{"healthcheck": map[string]any{"disable": true}}, []string{"image-entry", "image$$arg"}, nil},
		{"NONE health", map[string]any{"healthcheck": map[string]any{"test": []any{"NONE"}}}, []string{"image-entry", "image$$arg"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest, health, err := serviceFilesystemExecution(test.service, image)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(manifest.Command, test.command) || !slices.Equal(manifest.Healthcheck, test.health) {
				t.Fatalf("execution changed: %#v", manifest)
			}
			if health["interval"] != "5s" || health["retries"] != 2 {
				t.Fatalf("health timing lost: %#v", health)
			}
		})
	}
	for _, service := range []map[string]any{{"command": "unresolved"}, {"command": []any{7}}, {"entrypoint": []any{}, "command": []any{}}, {"healthcheck": map[string]any{"test": []any{"CMD-SHELL", "a", "b"}}}, {"healthcheck": map[string]any{"test": []any{"invalid"}}}} {
		if _, _, err := serviceFilesystemExecution(service, image); err == nil {
			t.Fatalf("accepted invalid command %#v", service)
		}
	}
}
