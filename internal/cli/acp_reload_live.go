//go:build cooplivetest

package cli

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/AndrewDryga/coop/internal/liveprocess"
)

// prepareACPReload preserves the authenticated outer-supervisor capability across its immediate
// self-exec. Child processes still inherit neither the activation environment nor an open fd 3.
func prepareACPReload() (func(), error) {
	rawFD := os.Getenv(liveprocess.ControlFDEnv)
	processDir := os.Getenv(liveprocess.ProcessDirEnv)
	cleanupID := os.Getenv(liveprocess.CleanupIDEnv)
	if rawFD == "" && processDir == "" && cleanupID == "" {
		return func() {}, nil
	}
	fd, err := strconv.Atoi(rawFD)
	if err != nil || fd != 3 || processDir == "" || !liveprocess.ValidCleanupID(cleanupID) || !liveprocess.ValidateControlFD(fd) {
		return nil, errors.New("invalid live process control")
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		return nil, errors.New("inspect live process control")
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags&^unix.FD_CLOEXEC); err != nil {
		return nil, errors.New("preserve live process control for reload")
	}
	return func() { _, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags) }, nil
}
