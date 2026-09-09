package networkgateway

import (
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func OpenBootClock() (*BootClock, error) {
	file, err := os.Open("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, Failure("clock_unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(file, 38))
	_ = file.Close()
	if err != nil || len(data) > 37 {
		return nil, Failure("clock_unavailable")
	}
	var current, children unix.Stat_t
	if unix.Stat("/proc/self/ns/time", &current) != nil || unix.Stat("/proc/self/ns/time_for_children", &children) != nil || current.Ino != children.Ino || current.Dev != children.Dev {
		return nil, Failure("clock_namespace_mismatch")
	}
	clock := &BootClock{domain: ClockDomain{BootID: strings.TrimSpace(string(data)), TimeNamespace: strconv.FormatUint(current.Ino, 10)}, read: func() (BootInstant, error) {
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
