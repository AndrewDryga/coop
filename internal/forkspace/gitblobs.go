package forkspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// BlobReader reads blobs one at a time from a single `git cat-file --batch`, so a scan that reads
// every file a fork changed starts one Git process, not one per file. It runs under the same view
// as the one-shot read it replaces, and holds one object in memory at a time: the caller names the
// largest it will take. Cat-file --batch prints the stored bytes, with no filter or textconv.
type BlobReader struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stderr  bytes.Buffer
	cleanup func()
	err     error // the first failure; the stream is out of step after it, so every later read fails
}

// GitBlobs is a BlobReader under dir's trusted view, as GitOutput reads.
func GitBlobs(ctx context.Context, dir string) (*BlobReader, error) {
	cmd, err := GitCommand(ctx, dir, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	return startBlobReader(cmd, func() {})
}

// ObserveBlobs is a BlobReader under a fresh observation snapshot, as ObserveGit reads.
func ObserveBlobs(ctx context.Context, repository string) (*BlobReader, error) {
	cmd, cleanup, err := observationGitCommand(ctx, repository, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	return startBlobReader(cmd, cleanup)
}

func startBlobReader(cmd *exec.Cmd, cleanup func()) (_ *BlobReader, err error) {
	reader := &BlobReader{cmd: cmd, cleanup: cleanup}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	cmd.Stderr = &reader.stderr
	if reader.stdin, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	reader.stdout = bufio.NewReader(stdout)
	if err = cmd.Start(); err != nil {
		return nil, GitStalled(cmd, err)
	}
	return reader, nil
}

// Read returns the content of the blob object, refusing one larger than limit bytes.
func (r *BlobReader) Read(object string, limit int64) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	content, err := r.read(object, limit)
	if err != nil {
		r.err = fmt.Errorf("git cat-file --batch: %w", err)
		if waitErr := r.Close(); waitErr != nil {
			r.err = errors.Join(r.err, waitErr)
		}
		return nil, r.err
	}
	return content, nil
}

func (r *BlobReader) read(object string, limit int64) ([]byte, error) {
	if !ValidObjectID(object) {
		return nil, fmt.Errorf("invalid object %q", object)
	}
	if _, err := io.WriteString(r.stdin, object+"\n"); err != nil {
		return nil, err
	}
	header, err := r.stdout.ReadString('\n')
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[0] != object {
		return nil, fmt.Errorf("object %s: %s", object, strings.TrimSpace(header))
	}
	if fields[1] != "blob" {
		return nil, fmt.Errorf("object %s is a %s, not a blob", object, fields[1])
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return nil, fmt.Errorf("object %s has an invalid size %q", object, fields[2])
	}
	if size > limit {
		return nil, fmt.Errorf("object %s is %d bytes, over the %d this read takes", object, size, limit)
	}
	content := make([]byte, size+1) // the content and the newline cat-file ends it with
	if _, err := io.ReadFull(r.stdout, content); err != nil {
		return nil, err
	}
	if content[size] != '\n' {
		return nil, fmt.Errorf("object %s did not end where its size says", object)
	}
	return content[:size], nil
}

// Close ends the Git process and removes its view. A reader that never failed reports how Git exited.
func (r *BlobReader) Close() error {
	if r.cmd == nil {
		return nil
	}
	cmd := r.cmd
	r.cmd = nil
	defer r.cleanup()
	_ = r.stdin.Close()
	err := GitStalled(cmd, cmd.Wait())
	if detail := strings.TrimSpace(r.stderr.String()); err != nil && detail != "" {
		err = fmt.Errorf("%w: %s", err, detail)
	}
	return err
}
