package liveprovider

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

type SupervisorCleanupSpec struct {
	Root, CIDDir, Supervisor, LabelKey string
	Phases                             []string
	OperationTimeout, QuietPeriod      time.Duration
	PollInterval                       time.Duration
}

type SupervisorCleanupOps struct {
	RemoveContainer func(context.Context, string) error
	RemoveByLabel   func(context.Context, string, string) (int, error)
	Now             func() time.Time
	Sleep           func(context.Context, time.Duration) error
}

// CleanupSupervisor best-effort removes known cidfile containers, then authoritatively sweeps the
// supervisor label across running and stopped state until it remains quiet for a full grace period.
// Call it once the process that launches boxes is gone: every box it started carries the label, and
// a runtime client still waiting on one exits when its container is removed.
func CleanupSupervisor(ctx context.Context, spec SupervisorCleanupSpec, ops SupervisorCleanupOps) error {
	if spec.Root == "" || spec.CIDDir == "" || spec.Supervisor == "" || spec.LabelKey == "" ||
		ops.RemoveContainer == nil || ops.RemoveByLabel == nil {
		return errors.New("incomplete live supervisor cleanup contract")
	}
	phases, err := cleanupPhases(spec.Phases)
	if spec.OperationTimeout <= 0 {
		spec.OperationTimeout = 2 * time.Second
	}
	if spec.QuietPeriod <= 0 {
		spec.QuietPeriod = time.Second
	}
	if spec.PollInterval <= 0 {
		spec.PollInterval = 100 * time.Millisecond
	}
	if ops.Now == nil {
		ops.Now = time.Now
	}
	if ops.Sleep == nil {
		ops.Sleep = sleepContext
	}

	result := err
	for _, phase := range phases {
		if id, ok := readContainerID(spec.Root, filepath.Join(spec.CIDDir, phase+".cid")); ok {
			operationCtx, cancel := context.WithTimeout(ctx, spec.OperationTimeout)
			_ = ops.RemoveContainer(operationCtx, id)
			cancel()
		}
	}

	quietSince := time.Time{}
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		sweepStarted := ops.Now()
		operationCtx, cancel := context.WithTimeout(ctx, spec.OperationTimeout)
		removed, err := ops.RemoveByLabel(operationCtx, spec.LabelKey, spec.Supervisor)
		cancel()
		if err != nil {
			return errors.Join(result, errors.New("remove live supervisor label"))
		}
		now := ops.Now()
		if removed > 0 {
			quietSince = time.Time{}
		} else if quietSince.IsZero() {
			quietSince = now
		} else if !sweepStarted.Before(quietSince.Add(spec.QuietPeriod)) {
			return result
		}
		if err := ops.Sleep(ctx, spec.PollInterval); err != nil {
			return errors.Join(result, err)
		}
	}
}

func cleanupPhases(phases []string) ([]string, error) {
	if len(phases) == 0 {
		return []string{"version", "prompt"}, nil
	}
	if len(phases) > 32 {
		return nil, errors.New("too many live cleanup phases")
	}
	result := make([]string, 0, len(phases))
	seen := map[string]bool{}
	for _, phase := range phases {
		if !validCleanupPhase(phase) || seen[phase] {
			return nil, errors.New("invalid live cleanup phase")
		}
		seen[phase] = true
		result = append(result, phase)
	}
	return result, nil
}

func validCleanupPhase(phase string) bool {
	if phase == "" || len(phase) > 64 || phase[0] == '-' || phase[len(phase)-1] == '-' {
		return false
	}
	for _, char := range phase {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func readContainerID(root, path string) (string, bool) {
	file, err := procharness.OpenRegularFile(root, path, os.O_RDONLY)
	if err != nil {
		return "", false
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 257))
	closeErr := file.Close()
	id := strings.TrimSpace(string(data))
	if readErr != nil || closeErr != nil || !validContainerID(id) {
		return "", false
	}
	return id, true
}

func validContainerID(id string) bool {
	if len(id) == 0 || len(id) > 256 {
		return false
	}
	for i, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || (i > 0 && (char == '_' || char == '.' || char == '-')) {
			continue
		}
		return false
	}
	return true
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
