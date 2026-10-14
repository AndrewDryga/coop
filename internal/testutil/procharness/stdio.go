package procharness

import (
	"errors"
	"io"
	"os"
	"os/exec"
)

// StartStdio gives the caller stdout ownership independent of cmd.Wait. StdoutPipe would
// let a background Wait close the reader before the final buffered frame was consumed.
// Configure environment, stderr and process attributes first; close/read the returned ends.
func StartStdio(cmd *exec.Cmd) (io.WriteCloser, *os.File, error) {
	if cmd.Stdout != nil {
		return nil, nil, errors.New("stdout already configured")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		reader.Close()
		writer.Close()
		return nil, nil, err
	}
	cmd.Stdout = writer
	if err := cmd.Start(); err != nil {
		stdin.Close()
		reader.Close()
		writer.Close()
		return nil, nil, err
	}
	writer.Close() // only the child can keep stdout open
	return stdin, reader, nil
}
