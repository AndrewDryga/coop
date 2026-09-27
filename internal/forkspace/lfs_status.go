package forkspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// RunLFSStatus filters verified hydrated files before applying the caller's output
// bound. command must emit porcelain-v2 --no-renames -z, and read must use its
// same trusted index. No repository-defined clean filter runs on the host.
func RunLFSStatus(ctx context.Context, workspace string, command *exec.Cmd, output io.Writer, read func(int, ...string) ([]byte, error)) error {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	pipe, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	defer pipe.Close()
	if err := command.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 8192), 16<<10)
	scanner.Split(splitGitRecord)
	for scanner.Scan() {
		record := scanner.Bytes()
		if verifiedLFSStatus(ctx, root, record, read) {
			if record[2] == '.' {
				continue
			}
			record[3] = '.' // Preserve a staged edit, but remove the false worktree edit.
		}
		if _, err = output.Write(append(record, 0)); err != nil {
			break
		}
	}
	err = errors.Join(err, scanner.Err(), ctx.Err())
	if err != nil {
		if command.Cancel != nil {
			_ = command.Cancel()
		} else {
			_ = command.Process.Kill()
		}
	}
	return errors.Join(err, command.Wait())
}

func verifiedLFSStatus(ctx context.Context, root *os.Root, record []byte, read func(int, ...string) ([]byte, error)) bool {
	fields := bytes.SplitN(record, []byte(" "), 9)
	if len(fields) != 9 || string(fields[0]) != "1" || len(fields[1]) != 2 || fields[1][1] != 'M' ||
		string(fields[2]) != "N..." ||
		!bytes.Equal(fields[4], fields[5]) {
		return false
	}
	blobID := string(fields[7])
	if !validPinnedCommit(blobID) {
		return false
	}
	path := string(fields[8])
	if !filepath.IsLocal(path) || path == "." {
		return false
	}
	attribute, err := read(len(path)+32, "check-attr", "-z", "--cached", "filter", "--", path)
	if err != nil || string(attribute) != path+"\x00filter\x00lfs\x00" {
		return false
	}
	pointer, err := read(1024, "cat-file", "blob", blobID)
	if err != nil {
		return false
	}
	oid, size, ok := ParseLFSPointer(pointer)
	if !ok {
		return false
	}
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return false
	}
	file, err := root.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() != size || !os.SameFile(info, opened) {
		return false
	}
	if err := copyLFS(ctx, io.Discard, file, size, oid); err != nil {
		return false
	}
	finalPath, err := root.Lstat(path)
	finalFile, finalErr := file.Stat()
	return err == nil && finalErr == nil && finalPath.Mode().IsRegular() && finalFile.Mode().IsRegular() &&
		os.SameFile(info, finalPath) && os.SameFile(opened, finalFile) &&
		finalPath.Size() == size && finalFile.Size() == size && finalPath.Mode() == info.Mode() &&
		finalFile.Mode() == opened.Mode() && finalPath.ModTime() == info.ModTime() && finalFile.ModTime() == opened.ModTime()
}
