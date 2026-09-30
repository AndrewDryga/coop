package sessionsvc

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// AdmitJobResources returns an isolated launch config after checking the accepted job against
// operator ceilings. A smaller operator limit refuses the job; it never rewrites job authority.
func AdmitJobResources(cfg *config.Config, rt runtime.Runtime, requested workerproto.JobResources) (*config.Config, error) {
	if cfg == nil || !requested.Valid() {
		return nil, errors.New("controller job has invalid resource limits")
	}
	if !rt.SupportsRunLimits() {
		return nil, errors.New("worker runtime cannot enforce controller job resource limits")
	}
	if cfg.CPUs != "" {
		ceiling, err := strconv.ParseFloat(cfg.CPUs, 64)
		if err != nil || math.IsNaN(ceiling) || math.IsInf(ceiling, 0) || ceiling <= 0 {
			return nil, errors.New("worker CPU ceiling is invalid")
		}
		if float64(requested.CPUMillis)/1000 > ceiling {
			return nil, errors.New("controller job exceeds the worker CPU ceiling")
		}
	}
	if cfg.Memory != "" {
		ceiling, err := workerMemoryCeiling(cfg.Memory)
		if err != nil {
			return nil, errors.New("worker memory ceiling is invalid")
		}
		if requested.MemoryBytes > ceiling {
			return nil, errors.New("controller job exceeds the worker memory ceiling")
		}
	}
	if cfg.Pids != "" && cfg.Pids != "0" && cfg.Pids != "unlimited" {
		ceiling, err := strconv.Atoi(cfg.Pids)
		if err != nil || ceiling <= 0 {
			return nil, errors.New("worker process ceiling is invalid")
		}
		if requested.PIDs > ceiling {
			return nil, errors.New("controller job exceeds the worker process ceiling")
		}
	}
	launch := cfg.Clone()
	launch.SetRuntimeLimits(
		strconv.FormatFloat(float64(requested.CPUMillis)/1000, 'f', 3, 64),
		strconv.FormatInt(requested.MemoryBytes, 10),
		strconv.Itoa(requested.PIDs),
	)
	return launch, nil
}

func workerMemoryCeiling(value string) (int64, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	multiplier := float64(1)
	for _, suffix := range []struct {
		name  string
		power int
	}{{"pb", 5}, {"p", 5}, {"tb", 4}, {"t", 4}, {"gb", 3}, {"g", 3}, {"mb", 2}, {"m", 2}, {"kb", 1}, {"k", 1}, {"b", 0}} {
		if strings.HasSuffix(value, suffix.name) {
			value = strings.TrimSuffix(value, suffix.name)
			multiplier = math.Pow(1024, float64(suffix.power))
			break
		}
	}
	number, err := strconv.ParseFloat(value, 64)
	bytes := number * multiplier
	if err != nil || math.IsNaN(bytes) || math.IsInf(bytes, 0) || bytes < 1 || bytes > math.MaxInt64 {
		return 0, fmt.Errorf("invalid memory ceiling")
	}
	return int64(bytes), nil
}
