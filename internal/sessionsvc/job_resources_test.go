package sessionsvc

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestAdmitJobResourcesUsesIsolatedEnforcedLimits(t *testing.T) {
	operator := &config.Config{CPUs: "2", Memory: "2g", Pids: "512"}
	requested := workerproto.JobResources{CPUMillis: 1500, MemoryBytes: 1 << 30, PIDs: 256}
	launch, err := AdmitJobResources(operator, runtime.Runtime{Name: "docker"}, requested)
	if err != nil {
		t.Fatal(err)
	}
	if launch.CPUs != "1.500" || launch.Memory != "1073741824" || launch.Pids != "256" ||
		!launch.Explicit("COOP_CPUS") || !launch.Explicit("COOP_MEMORY") || !launch.Explicit("COOP_PIDS") {
		t.Fatalf("admitted launch limits = %+v", launch)
	}
	if operator.CPUs != "2" || operator.Memory != "2g" || operator.Pids != "512" || operator.Explicit("COOP_CPUS") {
		t.Fatalf("operator settings were changed: %+v", operator)
	}
	for name, change := range map[string]func(*config.Config){
		"cpu":     func(cfg *config.Config) { cfg.CPUs = "1" },
		"memory":  func(cfg *config.Config) { cfg.Memory = "512m" },
		"pids":    func(cfg *config.Config) { cfg.Pids = "128" },
		"invalid": func(cfg *config.Config) { cfg.Memory = "not-a-size" },
	} {
		t.Run(name, func(t *testing.T) {
			ceiling := operator.Clone()
			change(ceiling)
			if _, err := AdmitJobResources(ceiling, runtime.Runtime{Name: "docker"}, requested); err == nil {
				t.Fatal("accepted excess or invalid operator ceiling")
			}
		})
	}
	if _, err := AdmitJobResources(operator, runtime.Runtime{Name: "container"}, requested); err == nil || !strings.Contains(err.Error(), "cannot enforce") {
		t.Fatalf("unsupported runtime = %v", err)
	}
}
