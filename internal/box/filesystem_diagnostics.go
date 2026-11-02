package box

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Capture useful evidence before failed-admission cleanup removes its private
// log. The original error remains discoverable with errors.Is/As.
func filesystemBuildError(cause error, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("read filesystem build diagnostic: %w", err))
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return errors.Join(cause, err)
	}
	const limit = 16 << 10
	data := make([]byte, min(info.Size(), limit))
	n, err := file.ReadAt(data, max(0, info.Size()-limit))
	if err != nil && !errors.Is(err, io.EOF) {
		return errors.Join(cause, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data[:n])), "\n")
	lines = lines[max(0, len(lines)-20):]
	for i := range lines {
		lines[i] = cleanLaunchText(lines[i])
	}
	detail := strings.TrimSpace(strings.Join(lines, "\n"))
	if detail == "" {
		return cause
	}
	return fmt.Errorf("%w\nFilesystem helper build output:\n%s", cause, detail)
}
