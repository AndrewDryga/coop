package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/processidentity"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

const (
	runtimeContainerVersion = 1
	runtimeRecordMaxBytes   = 8 << 10
)

type runtimeContainerRecord struct {
	Version    int               `json:"version"`
	ID         string            `json:"id"`
	PID        int               `json:"pid"`
	PGID       int               `json:"pgid"`
	StartToken string            `json:"start_token"`
	Labels     map[string]string `json:"labels"`
}

func runtimeContainerDir(root string) string {
	return filepath.Join(root, "state", "runtime-containers")
}

func runtimeContainerPath(root, id string) string {
	return filepath.Join(runtimeContainerDir(root), id+".json")
}

func registerRuntimeContainer(root string, rawLabels []string) (string, error) {
	pid := os.Getpid()
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid <= 1 {
		return "", errors.New("runtime process group is not inspectable")
	}
	token := processidentity.StartToken(pid)
	if !processidentity.Stable(token) {
		return "", errors.New("runtime process identity is not stable")
	}
	labels, err := runtimeLabels(rawLabels)
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("fixture-%d", pid)
	record := runtimeContainerRecord{
		Version: runtimeContainerVersion, ID: id, PID: pid, PGID: pgid,
		StartToken: token, Labels: labels,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	dir := runtimeContainerDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(runtimeContainerPath(root, id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(runtimeContainerPath(root, id))
		return "", err
	}
	return id, nil
}

func unregisterRuntimeContainer(root, id string) {
	_ = os.Remove(runtimeContainerPath(root, id))
}

func runtimeLabels(raw []string) (map[string]string, error) {
	labels := make(map[string]string, len(raw))
	for _, item := range raw {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" || value == "" || strings.ContainsAny(key+value, "\x00\r\n") {
			return nil, fmt.Errorf("invalid runtime label %q", item)
		}
		labels[key] = value
	}
	return labels, nil
}

func matchingRuntimeContainers(root string, filters map[string]string) ([]string, error) {
	entries, err := os.ReadDir(runtimeContainerDir(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, errors.New("invalid runtime container record")
		}
		record, err := readRuntimeContainer(root, strings.TrimSuffix(entry.Name(), ".json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		match := true
		for key, value := range filters {
			if record.Labels[key] != value {
				match = false
				break
			}
		}
		if match {
			ids = append(ids, record.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func removeRuntimeContainer(root, id string) error {
	record, err := readRuntimeContainer(root, id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	switch processidentity.Inspect(record.PID, record.StartToken) {
	case processidentity.Gone, processidentity.Mismatch:
		return removeRuntimeRecord(root, id)
	case processidentity.Unknown:
		return errors.New("runtime container process identity is not verifiable")
	case processidentity.Match:
		pgid, err := syscall.Getpgid(record.PID)
		if err != nil || pgid != record.PGID {
			return errors.New("runtime container process group changed")
		}
		currentPGID, err := syscall.Getpgid(0)
		if err != nil || currentPGID == record.PGID {
			return errors.New("refusing to signal the runtime fixture's own process group")
		}
		if err := syscall.Kill(-record.PGID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return removeRuntimeRecord(root, id)
	default:
		return errors.New("unknown runtime container process identity")
	}
}

func removeRuntimeRecord(root, id string) error {
	err := os.Remove(runtimeContainerPath(root, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func hasRuntimeLabel(raw []string, key string) bool {
	prefix := key + "="
	for _, item := range raw {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

func readRuntimeContainer(root, id string) (runtimeContainerRecord, error) {
	if !validRuntimeContainerID(id) {
		return runtimeContainerRecord{}, errors.New("invalid runtime container id")
	}
	path := runtimeContainerPath(root, id)
	f, err := procharness.OpenRegularFile(root, path, os.O_RDONLY)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return runtimeContainerRecord{}, os.ErrNotExist
		}
		return runtimeContainerRecord{}, errors.New("open runtime container record")
	}
	info, statErr := f.Stat()
	data, readErr := io.ReadAll(io.LimitReader(f, runtimeRecordMaxBytes+1))
	closeErr := f.Close()
	var stat *syscall.Stat_t
	statOK := false
	if statErr == nil {
		stat, statOK = info.Sys().(*syscall.Stat_t)
	}
	if statErr != nil || readErr != nil || closeErr != nil || len(data) > runtimeRecordMaxBytes ||
		!statOK || int(stat.Uid) != os.Getuid() || info.Mode().Perm() != 0o600 {
		return runtimeContainerRecord{}, errors.New("invalid runtime container record")
	}
	var record runtimeContainerRecord
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&record); err != nil || dec.Decode(&struct{}{}) != io.EOF ||
		record.Version != runtimeContainerVersion || record.ID != id || record.ID != fmt.Sprintf("fixture-%d", record.PID) || record.PID <= 1 ||
		record.PGID <= 1 || !processidentity.Stable(record.StartToken) || record.Labels == nil {
		return runtimeContainerRecord{}, errors.New("invalid runtime container record")
	}
	return record, nil
}

func validRuntimeContainerID(id string) bool {
	if !strings.HasPrefix(id, "fixture-") || len(id) > 64 {
		return false
	}
	for _, r := range strings.TrimPrefix(id, "fixture-") {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(strings.TrimPrefix(id, "fixture-")) > 0
}
