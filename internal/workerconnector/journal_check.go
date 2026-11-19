package workerconnector

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RecordCheck is what this build makes of one kind of durable record in the connector journal.
type RecordCheck struct {
	Noun    string   // one record, as a person reads it: "worker command receipt"
	Version int      // the one version this build reads
	Current int      // records this build reads as they are
	Newer   int      // records a newer build wrote
	Older   int      // records older than this build reads
	Damaged []string // records this build rejects at their own version: "<file>: <reason>"
}

// CheckJournal reads every record in the connector journal under dir with the readers the
// connector itself uses, so a record passes here exactly when a connector of this build accepts
// it. It never opens the journal, which creates and repairs its directories: a missing directory
// simply has no records.
func CheckJournal(dir string) ([]RecordCheck, error) {
	j := &journal{
		dir:     filepath.Join(dir, "commands"),
		streams: filepath.Join(dir, "event-streams"),
		origins: filepath.Join(dir, "create-origins"),
		settled: filepath.Join(dir, "create-origins", "settled"),
	}
	kinds := []struct {
		noun    string
		dirs    []string
		version int
		read    func(string) error
	}{
		{"worker command receipt", []string{j.dir}, journalVersion, func(path string) error { _, err := j.read(path); return err }},
		{"worker event stream", []string{j.streams}, eventStreamVersion, func(path string) error { _, err := j.readEventStream(path); return err }},
		{"worker create origin", []string{j.origins, j.settled}, createOriginVersion, func(path string) error { _, err := j.readCreateOrigin(path); return err }},
	}
	checks := make([]RecordCheck, 0, len(kinds))
	for _, kind := range kinds {
		check := RecordCheck{Noun: kind.noun, Version: kind.version}
		for _, dir := range kind.dirs {
			entries, err := os.ReadDir(dir)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("list %ss: %w", kind.noun, err)
			}
			for _, entry := range entries {
				// The connector's own listings skip everything else: temporaries, bodies and cursors.
				if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
					continue
				}
				path := filepath.Join(dir, entry.Name())
				readErr := kind.read(path)
				if readErr == nil {
					check.Current++
					continue
				}
				if errors.Is(readErr, os.ErrNotExist) {
					continue // removed since the listing
				}
				switch version, ok := recordVersion(path); {
				case ok && version > kind.version:
					check.Newer++
				case ok && version >= 1 && version < kind.version:
					check.Older++
				default:
					name, _ := filepath.Rel(kind.dirs[0], path)
					check.Damaged = append(check.Damaged, name+": "+readErr.Error())
				}
			}
		}
		checks = append(checks, check)
	}
	return checks, nil
}

// recordVersion is the version a record declares, when it parses and declares one.
func recordVersion(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var header struct {
		Version *int `json:"version"`
	}
	if json.Unmarshal(data, &header) != nil || header.Version == nil {
		return 0, false
	}
	return *header.Version, true
}
