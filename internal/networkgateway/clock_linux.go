package networkgateway

import (
	"io"
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

func OpenBootClock() (*BootClock, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	file, err := os.Open("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, Failure("clock_unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(file, 38))
	_ = file.Close()
	if err != nil || len(data) > 37 {
		return nil, Failure("clock_unavailable")
	}
	domain, err := qualifyClockDomain(strings.TrimSpace(string(data)), currentClockNamespace, func() ([]byte, error) {
		file, err := os.Open("/proc/self/timens_offsets")
		if err != nil {
			return nil, err
		}
		defer file.Close()
		return io.ReadAll(io.LimitReader(file, 4097))
	})
	if err != nil {
		return nil, err
	}
	clock := &BootClock{domain: domain, read: func() (BootInstant, error) {
		var value unix.Timespec
		if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &value); err != nil {
			return 0, Failure("clock_unavailable")
		}
		if value.Sec < 0 || value.Sec > (1<<63-1)/1_000_000_000 || value.Nsec < 0 || value.Nsec >= 1_000_000_000 {
			return 0, Failure("clock_unavailable")
		}
		seconds := value.Sec * 1_000_000_000
		if value.Nsec > (1<<63-1)-seconds {
			return 0, Failure("clock_unavailable")
		}
		return BootInstant(seconds + value.Nsec), nil
	}}
	if _, err := clock.Now(); err != nil {
		return nil, err
	}
	return clock, nil
}

func currentClockNamespace() (clockNamespace, error) {
	var selected clockNamespace
	// timens_offsets describes the leader's child namespace. Prove that it
	// also describes this pinned thread; /proc/self alone cannot establish that.
	for _, path := range []string{"/proc/self/ns/time", "/proc/self/ns/time_for_children", "/proc/thread-self/ns/time", "/proc/thread-self/ns/time_for_children"} {
		var stat unix.Stat_t
		if unix.Stat(path, &stat) != nil {
			return clockNamespace{}, Failure("clock_namespace_mismatch")
		}
		next := clockNamespace{device: uint64(stat.Dev), inode: stat.Ino}
		if selected.inode != 0 && selected != next {
			return clockNamespace{}, Failure("clock_namespace_mismatch")
		}
		selected = next
	}
	return selected, nil
}
