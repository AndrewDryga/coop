package procharness

import (
	"errors"
	"io"
	"os"
	"time"
)

// CaptureOutput is for serial tests that already own the selected process-global streams.
// Multiple streams share one pipe, preserving combined-terminal ordering. Drain before the
// callback writes; restore/close/join even when it panics or exits its test goroutine.
func CaptureOutput(fn func(), streams ...**os.File) (output string, err error) {
	if len(streams) == 0 {
		return "", errors.New("capture needs an output stream")
	}
	saved := make([]*os.File, len(streams))
	for i, stream := range streams {
		if stream == nil {
			return "", errors.New("capture has a nil stream")
		}
		saved[i] = *stream
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return "", err
	}
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, readErr := io.ReadAll(reader)
		done <- result{data, errors.Join(readErr, reader.Close())}
	}()
	for _, stream := range streams {
		*stream = writer
	}
	defer func() {
		for i, stream := range streams {
			*stream = saved[i]
		}
		err = errors.Join(err, writer.Close())
		select {
		case got := <-done:
			output, err = string(got.data), errors.Join(err, got.err)
		case <-time.After(time.Minute): // broken fixture guard, not a production grace period
			err = errors.Join(err, errors.New("capture reader did not drain"), reader.Close())
			select {
			case <-done:
			case <-time.After(time.Minute):
				err = errors.Join(err, errors.New("capture reader still blocked after close"))
			}
		}
	}()
	fn()
	return
}
