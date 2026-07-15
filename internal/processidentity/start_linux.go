//go:build linux

package processidentity

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func platformStartToken(pid int) string {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) == "" {
		return ""
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	start, ok := linuxStartTicks(b)
	if !ok {
		return ""
	}
	return "linux-proc-v1:" + strings.TrimSpace(string(boot)) + ":" + start
}

func linuxStartTicks(stat []byte) (string, bool) {
	// comm is parenthesized and may contain spaces or ')', so fields begin after the final ')'.
	endComm := strings.LastIndexByte(string(stat), ')')
	if endComm < 0 {
		return "", false
	}
	fields := strings.Fields(string(stat[endComm+1:]))
	if len(fields) <= 19 { // field 3 is index 0 here; process starttime is field 22/index 19
		return "", false
	}
	start := fields[19]
	if _, err := strconv.ParseUint(start, 10, 64); err != nil {
		return "", false
	}
	return start, true
}
