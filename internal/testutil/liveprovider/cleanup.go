package liveprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/liveprocess"
	"github.com/AndrewDryga/coop/internal/processidentity"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

type SupervisorCleanupSpec struct {
	Root, CIDDir, ProcessDir, Supervisor, LabelKey string
	Phases                                         []string
	OperationTimeout, ProcessGrace, QuietPeriod    time.Duration
	PollInterval                                   time.Duration
}

type SupervisorCleanupOps struct {
	RemoveContainer func(context.Context, string) error
	RemoveByLabel   func(context.Context, string, string) (int, error)
	Now             func() time.Time
	Sleep           func(context.Context, time.Duration) error
}

// CleanupSupervisor best-effort removes known cidfile containers, then authoritatively sweeps the
// supervisor label across running and stopped state until it remains quiet for a full grace period.
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
	if spec.ProcessGrace <= 0 {
		spec.ProcessGrace = 500 * time.Millisecond
	}
	if ops.Now == nil {
		ops.Now = time.Now
	}
	if ops.Sleep == nil {
		ops.Sleep = sleepContext
	}

	result := err
	producersQuiesced := true
	if spec.ProcessDir != "" {
		var processErr error
		producersQuiesced, processErr = terminateRegisteredGroups(ctx, spec, ops)
		result = errors.Join(result, processErr)
	}

	for _, phase := range phases {
		if id, ok := readContainerID(spec.Root, filepath.Join(spec.CIDDir, phase+".cid")); ok {
			operationCtx, cancel := context.WithTimeout(ctx, spec.OperationTimeout)
			_ = ops.RemoveContainer(operationCtx, id)
			cancel()
		}
	}

	quietSince := time.Time{}
	labelErrRecorded := false
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		sweepStarted := ops.Now()
		operationCtx, cancel := context.WithTimeout(ctx, spec.OperationTimeout)
		removed, err := ops.RemoveByLabel(operationCtx, spec.LabelKey, spec.Supervisor)
		cancel()
		if err != nil {
			if !labelErrRecorded {
				result = errors.Join(result, errors.New("remove live supervisor label"))
				labelErrRecorded = true
			}
			if producersQuiesced {
				return result
			}
			quietSince = time.Time{}
			if err := ops.Sleep(ctx, spec.PollInterval); err != nil {
				return errors.Join(result, err)
			}
			continue
		}
		now := ops.Now()
		if removed > 0 {
			quietSince = time.Time{}
		} else if quietSince.IsZero() {
			quietSince = now
		} else if producersQuiesced && !sweepStarted.Before(quietSince.Add(spec.QuietPeriod)) {
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

func terminateRegisteredGroups(ctx context.Context, spec SupervisorCleanupSpec, ops SupervisorCleanupOps) (bool, error) {
	groups, records, scanErr := readProcessRecords(spec.Root, spec.ProcessDir, spec.Supervisor)
	var result error
	result = errors.Join(result, scanErr)
	for _, pgid := range groups {
		if pgid == syscall.Getpgrp() {
			result = errors.Join(result, errors.New("live process registry contains cleanup group"))
			continue
		}
		match, err := authorizeProcessGroup(pgid, records[pgid])
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if match {
			if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				result = errors.Join(result, errors.New("signal live process group"))
			}
		}
	}
	if err := waitRegisteredGroups(ctx, groups, spec.ProcessGrace, ops); err != nil {
		result = errors.Join(result, err)
	}
	for _, pgid := range groups {
		if !processGroupAlive(pgid) {
			continue
		}
		match, err := authorizeProcessGroup(pgid, records[pgid])
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if match {
			if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				result = errors.Join(result, errors.New("kill live process group"))
			}
		}
	}
	if err := waitRegisteredGroups(ctx, groups, spec.OperationTimeout, ops); err != nil {
		result = errors.Join(result, err)
	}
	quiesced := scanErr == nil && ctx.Err() == nil
	for _, pgid := range groups {
		if processGroupAlive(pgid) {
			quiesced = false
			result = errors.Join(result, errors.New("live process group survived cleanup"))
		}
	}
	return quiesced, result
}

func readProcessRecords(root, processDir, supervisor string) ([]int, map[int][]liveprocess.Record, error) {
	if err := validatePrivateControlDir(root, processDir); err != nil {
		return nil, nil, err
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil, errors.New("open live process registry root")
	}
	rel, err := filepath.Rel(canonicalRoot, processDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, nil, errors.New("invalid live process registry")
	}
	rootHandle, err := os.OpenRoot(canonicalRoot)
	if err != nil {
		return nil, nil, errors.New("open live process registry root")
	}
	defer rootHandle.Close()
	registry, err := rootHandle.OpenRoot(rel)
	if err != nil {
		return nil, nil, errors.New("open live process registry")
	}
	defer registry.Close()
	dir, err := registry.Open(".")
	if err != nil {
		return nil, nil, errors.New("read live process registry")
	}
	entries, readErr := dir.ReadDir(liveprocess.MaxRecords + 1)
	closeErr := dir.Close()
	if (readErr != nil && !errors.Is(readErr, io.EOF)) || closeErr != nil {
		return nil, nil, errors.New("read live process registry")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	groups := map[int][]liveprocess.Record{}
	var result error
	if len(entries) > liveprocess.MaxRecords {
		result = errors.New("live process registry exceeds limit")
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".pending-") {
			if entry.IsDir() || registry.Remove(name) != nil {
				result = errors.Join(result, errors.New("invalid pending live process record"))
			}
			continue
		}
		if !strings.HasSuffix(name, ".json") || entry.IsDir() {
			result = errors.Join(result, errors.New("invalid live process record"))
			continue
		}
		record, err := readProcessRecord(root, filepath.Join(processDir, name), supervisor)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		groups[record.PGID] = append(groups[record.PGID], record)
	}
	pgids := make([]int, 0, len(groups))
	for pgid := range groups {
		pgids = append(pgids, pgid)
	}
	sort.Ints(pgids)
	return pgids, groups, result
}

func readProcessRecord(root, path, supervisor string) (liveprocess.Record, error) {
	file, err := procharness.OpenRegularFile(root, path, os.O_RDONLY)
	if err != nil {
		return liveprocess.Record{}, errors.New("open live process record")
	}
	info, statErr := file.Stat()
	data, readErr := io.ReadAll(io.LimitReader(file, liveprocess.MaxRecordSize+1))
	closeErr := file.Close()
	var stat *syscall.Stat_t
	statOK := false
	if statErr == nil {
		stat, statOK = info.Sys().(*syscall.Stat_t)
	}
	if statErr != nil || readErr != nil || closeErr != nil || len(data) > liveprocess.MaxRecordSize ||
		!statOK || int(stat.Uid) != os.Getuid() || info.Mode().Perm() != 0o600 {
		return liveprocess.Record{}, errors.New("invalid live process record")
	}
	var record liveprocess.Record
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		record.Schema != liveprocess.RecordSchema || record.PID <= 1 || record.PID != record.PGID ||
		record.UID != os.Getuid() || !processidentity.Stable(record.StartToken) ||
		record.Supervisor != supervisor {
		return liveprocess.Record{}, errors.New("invalid live process record")
	}
	return record, nil
}

func authorizeProcessGroup(pgid int, records []liveprocess.Record) (bool, error) {
	if !processGroupAlive(pgid) {
		return false, nil
	}
	unknown := false
	for _, record := range records {
		switch processidentity.Inspect(record.PID, record.StartToken) {
		case processidentity.Match:
			current, err := syscall.Getpgid(record.PID)
			if err == nil && current == pgid {
				return true, nil
			}
			unknown = true
		case processidentity.Unknown:
			unknown = true
		}
	}
	if unknown {
		return false, errors.New("live process identity is unreadable")
	}
	return false, errors.New("live process group has no matching identity")
}

func waitRegisteredGroups(ctx context.Context, groups []int, timeout time.Duration, ops SupervisorCleanupOps) error {
	deadline := ops.Now().Add(timeout)
	for {
		alive := false
		for _, pgid := range groups {
			alive = alive || processGroupAlive(pgid)
		}
		if !alive || !ops.Now().Before(deadline) {
			return nil
		}
		if err := ops.Sleep(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
}

func processGroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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
