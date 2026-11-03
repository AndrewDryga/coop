package sessionsvc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/safefile"
)

// CompanionSnapshotCheck is a read-only inventory for the stopped-worker
// upgrade check. Cache ownership receipts are versioned from their first write.
type CompanionSnapshotCheck struct {
	Version int
	Current int
	Newer   int
	Older   int
	Damaged []string
}

func CheckCompanionSnapshots(stateRoot string) (CompanionSnapshotCheck, error) {
	result := CompanionSnapshotCheck{Version: companionSnapshotVersion}
	canonical, err := filepath.EvalSymlinks(stateRoot)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	parent := filepath.Join(canonical, companionSnapshotRoot)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || (!validSessionDigest(entry.Name()) && !strings.HasPrefix(entry.Name(), ".snapshot-")) {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		if _, err := readCompanionSnapshotReceipt(path); err == nil {
			result.Current++
			continue
		}
		pinned, err := safefile.OpenRoot(path)
		if err != nil {
			result.Damaged = append(result.Damaged, entry.Name()+": unreadable snapshot ownership")
			continue
		}
		data, err := safefile.ReadRegular(pinned, "receipt.json", 4096)
		_ = pinned.Close()
		var header struct {
			Version int `json:"version"`
		}
		if err != nil || json.Unmarshal(data, &header) != nil || header.Version < 1 {
			result.Damaged = append(result.Damaged, entry.Name()+": unreadable snapshot receipt")
			continue
		}
		switch {
		case header.Version > companionSnapshotVersion:
			result.Newer++
		case header.Version < companionSnapshotVersion:
			result.Older++
		default:
			result.Damaged = append(result.Damaged, entry.Name()+": snapshot ownership changed")
		}
	}
	return result, nil
}
