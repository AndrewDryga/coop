package forkspace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type LFSPointer struct {
	Path string
	OID  string
	Size int64
	Mode os.FileMode
	data []byte
}

// VisitLFSPointers streams a pinned checkout's tree, attributes and small blobs.
// Neither worktree attributes nor executable Git filters determine its result.
func VisitLFSPointers(ctx context.Context, repository, commit string, visit func(LFSPointer) error) error {
	if !validPinnedCommit(commit) {
		return errors.New("invalid LFS source commit")
	}
	_, err := ObserveGit(ctx, repository,
		"diff-index", "--cached", "--quiet", "--no-ext-diff", "--no-textconv", "--ignore-submodules=dirty", commit, "--")
	if err != nil {
		return errors.New("LFS checkout index does not match the pinned tree")
	}
	blobs, err := startLFSReader(ctx, repository, "cat-file", "--batch")
	if err != nil {
		return err
	}
	defer blobs.close()
	attributes, err := startLFSReaderAt(ctx, repository, commit, "check-attr", "--cached", "-z", "--stdin", "filter")
	if err != nil {
		return err
	}
	defer attributes.close()
	listing, err := startLFSReader(ctx, repository, "ls-tree", "-r", "-l", "-z", commit)
	if err != nil {
		return err
	}
	defer listing.close()
	for {
		record, err := listing.output.ReadSlice(0)
		if errors.Is(err, io.EOF) && len(record) == 0 {
			return errors.Join(listing.command.Wait(), ctx.Err())
		}
		if err != nil {
			return err
		}
		header, path, ok := strings.Cut(strings.TrimSuffix(string(record), "\x00"), "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 4 {
			return errors.New("malformed LFS source tree entry")
		}
		if fields[0] != "100644" && fields[0] != "100755" {
			continue
		}
		size, err := strconv.Atoi(fields[3])
		if err != nil || size < 0 {
			return errors.New("invalid LFS source blob size")
		}
		if _, err := io.WriteString(attributes.input, path+"\x00"); err != nil {
			return err
		}
		for _, expected := range []string{path + "\x00", "filter\x00"} {
			got, err := attributes.output.ReadString(0)
			if err != nil || got != expected {
				return errors.New("LFS source attribute identity changed")
			}
		}
		attribute, err := attributes.output.ReadString(0)
		if err != nil {
			return err
		}
		if attribute != "lfs\x00" {
			continue
		}
		if _, err := fmt.Fprintln(blobs.input, fields[2]); err != nil {
			return err
		}
		blobHeader, err := blobs.output.ReadString('\n')
		if err != nil || blobHeader != fields[2]+" blob "+strconv.Itoa(size)+"\n" {
			return errors.New("LFS source blob identity changed")
		}
		// Inspect a bounded prefix even outside canonical pointer sizes. Otherwise
		// truncated or extended pointers silently become the supposed file payload.
		data := make([]byte, min(size, 1025))
		if _, err := io.ReadFull(blobs.output, data); err != nil {
			return err
		}
		if _, err := io.CopyN(io.Discard, blobs.output, int64(size-len(data))); err != nil {
			return err
		}
		if end, err := blobs.output.ReadByte(); err != nil || end != '\n' {
			return errors.New("incomplete LFS source blob")
		}
		if !looksLikeLFSPointer(data) {
			continue
		}
		oid, length, ok := ParseLFSPointer(data)
		if !ok || size > 1024 {
			return fmt.Errorf("unsupported LFS pointer at %q", path)
		}
		mode := os.FileMode(0644)
		if fields[0] == "100755" {
			mode = 0755
		}
		if err := visit(LFSPointer{Path: path, OID: oid, Size: length, Mode: mode, data: data}); err != nil {
			return err
		}
	}
}

func looksLikeLFSPointer(data []byte) bool {
	for _, version := range []string{"https://git-lfs.github.com/spec/v1", "https://hawser.github.com/spec/v1", "http://git-media.io/v/2"} {
		if bytes.HasPrefix(bytes.TrimSpace(data), []byte("version "+version)) {
			return true
		}
	}
	return false
}

type lfsReader struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *bufio.Reader
	cleanup func()
}

func startLFSReader(ctx context.Context, repository string, args ...string) (*lfsReader, error) {
	return startLFSReaderAt(ctx, repository, "", args...)
}

func startLFSReaderAt(ctx context.Context, repository, commit string, args ...string) (*lfsReader, error) {
	command, cleanup, err := observationGitCommand(ctx, repository, args...)
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			cleanup()
		}
	}()
	command.Env = append(command.Env, "GIT_FLUSH=1", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.attributesFile", "GIT_CONFIG_VALUE_0="+os.DevNull)
	if commit != "" {
		// Cached attributes must use the exact tree, not a later source-index snapshot.
		// This read-tree writes only the observation's private disposable index.
		readArgs := append(append(append([]string{"-C", repository}, GitHardening...), gitViewHardening...), "read-tree", commit)
		index := exec.CommandContext(ctx, "git", readArgs...)
		index.Env = command.Env
		if err := index.Run(); err != nil {
			return nil, err
		}
	}
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return nil, err
	}
	if err := command.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, err
	}
	started = true
	return &lfsReader{command: command, input: input, output: bufio.NewReaderSize(output, 16<<10), cleanup: cleanup}, nil
}

func (reader *lfsReader) close() {
	_ = reader.input.Close()
	_ = reader.command.Process.Kill()
	_ = reader.command.Wait()
	reader.cleanup()
}

// HydrateLFS copies only the pinned tree's required objects into an unpublished
// checkout's own storage. Existing non-pointer work is never overwritten.
func HydrateLFS(ctx context.Context, repository, commit string, sources ...string) error {
	return hydrateLFS(ctx, repository, commit, false, sources...)
}

func hydrateLFS(ctx context.Context, repository, commit string, publication bool, sources ...string) error {
	root, err := os.OpenRoot(repository)
	if err != nil {
		return err
	}
	defer root.Close()
	objects, err := openLFSMetadata(repository)
	if err != nil {
		return err
	}
	defer objects.Close()
	return VisitLFSPointers(ctx, repository, commit, func(pointer LFSPointer) error {
		object := lfsMetadataObjectPath(pointer.OID)
		if err := ensureLFSObject(ctx, objects, pointer, sources); err != nil {
			return err
		}
		info, err := root.Lstat(pointer.Path)
		if err != nil || !info.Mode().IsRegular() || (info.Mode().Perm()&0111 != 0) != (pointer.Mode&0111 != 0) {
			return fmt.Errorf("refusing redirected or changed LFS file mode at %q", pointer.Path)
		}
		if lfsFileMatches(ctx, root, pointer.Path, pointer) {
			return nil
		}
		file, err := root.Open(pointer.Path)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 1025))
		info, statErr := file.Stat()
		_ = file.Close()
		if readErr != nil || statErr != nil || !info.Mode().IsRegular() || !bytes.Equal(data, pointer.data) {
			return fmt.Errorf("refusing to overwrite changed LFS file %q", pointer.Path)
		}
		var beforeRename func() error
		if publication {
			beforeRename = func() error { return requireLFSPointer(root, pointer, info) }
		}
		return copyLFSFile(ctx, objects, object, root, pointer.Path, pointer.Mode, pointer, beforeRename)
	})
}

// CopyLFSObjects retains the pinned index's payloads without a second worktree.
// It also verifies existing objects when called without sources.
func CopyLFSObjects(ctx context.Context, repository, commit string, sources ...string) error {
	root, err := openLFSMetadata(repository)
	if err != nil {
		return err
	}
	defer root.Close()
	return VisitLFSPointers(ctx, repository, commit, func(pointer LFSPointer) error {
		return ensureLFSObject(ctx, root, pointer, sources)
	})
}

func ensureLFSObject(ctx context.Context, root *os.Root, pointer LFSPointer, sources []string) error {
	object := lfsMetadataObjectPath(pointer.OID)
	if lfsFileMatches(ctx, root, object, pointer) {
		return nil
	}
	// Git LFS does not transfer the empty object. Its identity is locally provable.
	if pointer.Size == 0 && pointer.OID == fmt.Sprintf("%x", sha256.Sum256(nil)) {
		return writeLFSFile(ctx, root, object, 0600, bytes.NewReader(nil), pointer, nil)
	}
	for _, source := range sources {
		sourceRoot, err := openLFSMetadata(source)
		if err != nil {
			return err
		}
		err = copyLFSFile(ctx, sourceRoot, object, root, object, 0600, pointer, nil)
		_ = sourceRoot.Close()
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return fmt.Errorf("missing verified LFS object %s", pointer.OID)
}

func VerifyLFS(ctx context.Context, repository, commit string) error {
	root, err := os.OpenRoot(repository)
	if err != nil {
		return err
	}
	defer root.Close()
	objects, err := openLFSMetadata(repository)
	if err != nil {
		return err
	}
	defer objects.Close()
	return VisitLFSPointers(ctx, repository, commit, func(pointer LFSPointer) error {
		info, err := root.Lstat(pointer.Path)
		if err != nil || !info.Mode().IsRegular() || (info.Mode().Perm()&0111 != 0) != (pointer.Mode&0111 != 0) ||
			!lfsFileMatches(ctx, root, pointer.Path, pointer) || !lfsFileMatches(ctx, objects, lfsMetadataObjectPath(pointer.OID), pointer) {
			return fmt.Errorf("incomplete or modified LFS file %q", pointer.Path)
		}
		return nil
	})
}

func lfsObjectPath(oid string) string {
	return filepath.Join(".git", lfsMetadataObjectPath(oid))
}

func lfsMetadataObjectPath(oid string) string {
	return filepath.Join("lfs", "objects", oid[:2], oid[2:4], oid)
}

// Native gitfiles and linked worktrees keep LFS in the common metadata store.
// Resolve that directory as data; never let a repository command choose a host path.
func openLFSMetadata(repository string) (*os.Root, error) {
	roots, err := GitMetadataDirectories(repository)
	if err != nil {
		return nil, err
	}
	return os.OpenRoot(roots[len(roots)-1])
}

// PublishIsolatedLFS follows the journaled tree checkout, before its ref CAS. No
// reset or filter runs: only exact committed pointers can become verified payloads.
// A failure may leave partial publication and must retain the caller's journal.
func PublishIsolatedLFS(ctx context.Context, source, repository, commit string) error {
	if err := hydrateLFS(ctx, repository, commit, true, source); err != nil {
		return err
	}
	return VerifyLFS(ctx, repository, commit)
}

func requireLFSPointer(root *os.Root, pointer LFSPointer, original os.FileInfo) error {
	info, err := root.Lstat(pointer.Path)
	if err != nil || !os.SameFile(info, original) || info.Mode() != original.Mode() {
		return errors.Join(fmt.Errorf("LFS publication target %q changed while staging", pointer.Path), err)
	}
	file, err := root.Open(pointer.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(original, opened) || opened.Mode() != original.Mode() {
		return errors.Join(fmt.Errorf("LFS publication target %q changed while opening", pointer.Path), err)
	}
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil || !bytes.Equal(data, pointer.data) {
		return errors.Join(fmt.Errorf("LFS publication target %q changed while staging", pointer.Path), err)
	}
	finalFile, statErr := file.Stat()
	finalPath, pathErr := root.Lstat(pointer.Path)
	if statErr != nil || pathErr != nil || !sameLFSFile(original, finalFile) || !sameLFSFile(original, finalPath) {
		return errors.Join(fmt.Errorf("LFS publication target %q changed while checking", pointer.Path), statErr, pathErr)
	}
	return nil
}

// Native hydrated files differ from their index blobs when filters are disabled.
// Before the journaled read-tree, replace only changed, verified old LFS payloads
// with their exact pointers. No filter or reset runs; a later failure is partial
// publication and the journal must stay. Unchanged LFS files remain hydrated.
func PrepareIsolatedLFSCheckout(ctx context.Context, repository, before, publication string) error {
	root, err := os.OpenRoot(repository)
	if err != nil {
		return err
	}
	defer root.Close()
	return VisitLFSPointers(ctx, repository, before, func(pointer LFSPointer) error {
		old, err := ObserveGit(ctx, repository, "ls-tree", "-z", before, "--", pointer.Path)
		if err != nil {
			return err
		}
		next, err := ObserveGit(ctx, repository, "ls-tree", "-z", publication, "--", pointer.Path)
		if err != nil || bytes.Equal(old, next) {
			return err
		}
		info, err := root.Lstat(pointer.Path)
		if err != nil || !info.Mode().IsRegular() || (info.Mode().Perm()&0111 != 0) != (pointer.Mode&0111 != 0) {
			return errors.Join(errors.New("changed LFS checkout mode before publication"), err)
		}
		if requireLFSPointer(root, pointer, info) == nil {
			return nil
		}
		if !lfsFileMatches(ctx, root, pointer.Path, pointer) {
			return fmt.Errorf("LFS checkout %q changed before publication", pointer.Path)
		}
		guard := func() error {
			current, err := root.Lstat(pointer.Path)
			if err != nil || !os.SameFile(info, current) || info.Mode() != current.Mode() || !lfsFileMatches(ctx, root, pointer.Path, pointer) {
				return errors.Join(fmt.Errorf("LFS checkout %q changed while staging", pointer.Path), err)
			}
			return nil
		}
		digest := sha256.Sum256(pointer.data)
		content := LFSPointer{OID: fmt.Sprintf("%x", digest), Size: int64(len(pointer.data))}
		return writeLFSFile(ctx, root, pointer.Path, pointer.Mode, bytes.NewReader(pointer.data), content, guard)
	})
}

func lfsFileMatches(ctx context.Context, root *os.Root, name string, pointer LFSPointer) bool {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	file, err := root.Open(name)
	if err != nil {
		return false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameLFSFile(info, opened) || opened.Size() != pointer.Size || copyLFS(ctx, io.Discard, file, pointer.Size, pointer.OID) != nil {
		return false
	}
	finalFile, statErr := file.Stat()
	finalPath, pathErr := root.Lstat(name)
	return statErr == nil && pathErr == nil && sameLFSFile(info, finalFile) && sameLFSFile(info, finalPath)
}

func sameLFSFile(before, after os.FileInfo) bool {
	return before != nil && after != nil && after.Mode().IsRegular() && os.SameFile(before, after) &&
		before.Mode() == after.Mode() && before.Size() == after.Size() && before.ModTime() == after.ModTime()
}

func copyLFSFile(ctx context.Context, source *os.Root, from string, destination *os.Root, to string, mode os.FileMode, pointer LFSPointer, beforeRename func() error) error {
	file, err := source.Open(from)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != pointer.Size {
		return errors.New("LFS object is not a regular file of the expected size")
	}
	return writeLFSFile(ctx, destination, to, mode, file, pointer, beforeRename)
}

func writeLFSFile(ctx context.Context, destination *os.Root, to string, mode os.FileMode, reader io.Reader, pointer LFSPointer, beforeRename func() error) error {
	if err := destination.MkdirAll(filepath.Dir(to), 0700); err != nil {
		return err
	}
	stage := filepath.Join(filepath.Dir(to), ".coop-lfs-"+rand.Text())
	output, err := destination.OpenFile(stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer destination.Remove(stage)
	writeErr := copyLFS(ctx, output, reader, pointer.Size, pointer.OID)
	err = errors.Join(writeErr, output.Sync(), output.Close())
	if err != nil {
		return err
	}
	if beforeRename != nil {
		if err := beforeRename(); err != nil {
			return err
		}
	}
	return destination.Rename(stage, to)
}

func copyLFS(ctx context.Context, writer io.Writer, reader io.Reader, size int64, oid string) error {
	digest := sha256.New()
	buffer := make([]byte, 64<<10)
	written := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := reader.Read(buffer)
		written += int64(n)
		if written > size {
			return errors.New("LFS object exceeds its declared size")
		}
		if n > 0 {
			_, _ = digest.Write(buffer[:n])
			if count, err := writer.Write(buffer[:n]); err != nil || count != n {
				return errors.Join(err, io.ErrShortWrite)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil || n == 0 {
			return errors.Join(readErr, io.ErrNoProgress)
		}
	}
	if written != size || fmt.Sprintf("%x", digest.Sum(nil)) != oid {
		return errors.New("LFS object does not match its SHA-256 and size")
	}
	return nil
}

func ParseLFSPointer(pointer []byte) (string, int64, bool) {
	lines := strings.Split(string(pointer), "\n")
	if len(lines) != 4 || lines[3] != "" || lines[0] != "version https://git-lfs.github.com/spec/v1" ||
		!strings.HasPrefix(lines[1], "oid sha256:") || !strings.HasPrefix(lines[2], "size ") {
		return "", 0, false
	}
	oid := strings.TrimPrefix(lines[1], "oid sha256:")
	if len(oid) != 64 || !validPinnedCommit(oid) {
		return "", 0, false
	}
	text := strings.TrimPrefix(lines[2], "size ")
	if text == "" || len(text) > 1 && text[0] == '0' {
		return "", 0, false
	}
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return "", 0, false
		}
	}
	size, err := strconv.ParseInt(text, 10, 64)
	return oid, size, err == nil && size >= 0
}
