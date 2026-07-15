// Package liveprocess defines the private process-control contract used only by tagged live tests.
package liveprocess

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	ControlFDEnv        = "COOP_TEST_LIVE_CONTROL_FD"
	ProcessDirEnv       = "COOP_TEST_LIVE_PROCESS_DIR"
	CleanupIDEnv        = "COOP_TEST_LIVE_CLEANUP_ID"
	RevokePathEnv       = "COOP_TEST_LIVE_REVOKE_PATH"
	ControlMarker       = "coop-live-process-control-v1\n"
	RecordSchema        = 1
	MaxRecords          = 128 // includes one pending hardlink-publication slot
	MaxPublishedRecords = MaxRecords - 1
	MaxRecordSize       = 4096
)

// Record authorises one test-owned ACP generation process group. StartToken is a stable kernel
// identity for the resident group leader; Supervisor binds the record to one cleanup label.
type Record struct {
	Schema     int    `json:"schema"`
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid"`
	UID        int    `json:"uid"`
	StartToken string `json:"start_token"`
	Supervisor string `json:"supervisor"`
}

func ValidCleanupID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '.' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func ValidRevocationPath(configDir, revokePath string) bool {
	configDir = filepath.Clean(configDir)
	revokePath = filepath.Clean(revokePath)
	if !filepath.IsAbs(configDir) || !filepath.IsAbs(revokePath) ||
		filepath.Dir(configDir) != filepath.Dir(revokePath) {
		return false
	}
	suffix := strings.TrimPrefix(filepath.Base(revokePath), ".coop-live-revoked-")
	if len(suffix) != 32 {
		return false
	}
	for _, char := range suffix {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// ValidateControlFD authenticates the inherited, private control file and marks it close-on-exec so
// the capability cannot reach a runtime CLI or provider process.
func ValidateControlFD(fd int) bool {
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG ||
		stat.Mode&0o077 != 0 || stat.Nlink != 1 || int(stat.Uid) != os.Getuid() {
		return false
	}
	data := make([]byte, len(ControlMarker)+1)
	n, err := syscall.Pread(fd, data, 0)
	if err != nil || n != len(ControlMarker) || string(data[:n]) != ControlMarker {
		return false
	}
	syscall.CloseOnExec(fd)
	return true
}
