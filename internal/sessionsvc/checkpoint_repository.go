package sessionsvc

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/secretscan"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Raw typed objects, not a compressed Git pack or binary patch: the controller's
// streaming credential scan must see even content later deleted from history.
func writeCheckpointRepository(ctx context.Context, workspace, base, head, tree string, env []string, output io.Writer) error {
	command := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "git", gitArgs(workspace, args)...)
		cmd.Env = env
		return cmd
	}
	var private string
	for _, value := range env {
		if strings.HasPrefix(value, "GIT_DIR=") {
			private = strings.TrimPrefix(value, "GIT_DIR=")
		}
	}
	if !filepath.IsAbs(private) {
		return errors.New("checkpoint snapshot has no private object store")
	}
	wantLinks, err := forkspace.Gitlinks(ctx, workspace, base)
	if err != nil {
		return err
	}
	links, err := forkspace.Gitlinks(ctx, private, tree)
	if err != nil || !maps.Equal(wantLinks, links) {
		return errors.New("nested work needs separate custody before checkpoint")
	}
	writer := tar.NewWriter(output)
	listing := command("rev-list", "--objects", "--no-object-names", head, tree, "^"+base)
	objects, err := listing.StdoutPipe()
	if err != nil {
		return err
	}
	defer objects.Close()
	if err := listing.Start(); err != nil {
		return err
	}
	defer func() { _ = listing.Process.Kill(); _ = listing.Wait() }()
	batch := command("cat-file", "--batch")
	batch.Stdin = objects
	pipe, err := batch.StdoutPipe()
	if err != nil {
		return err
	}
	defer pipe.Close()
	if err := batch.Start(); err != nil {
		return err
	}
	defer func() { _ = batch.Process.Kill(); _ = batch.Wait() }()
	reader := bufio.NewReader(pipe)
	for {
		header, err := reader.ReadSlice('\n')
		if errors.Is(err, io.EOF) && len(header) == 0 {
			break
		}
		if err != nil {
			return err
		}
		fields := strings.Fields(string(header))
		if len(fields) != 3 || !validSessionReviewObject(fields[0]) ||
			(fields[1] != "commit" && fields[1] != "tree" && fields[1] != "blob") {
			return errors.New("invalid checkpoint Git object")
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return errors.New("invalid checkpoint object size")
		}
		if err := checkpointTarHeader(writer, "objects/"+fields[1]+"/"+fields[0], 0644, size); err != nil {
			return err
		}
		objectHash := sha1.New()
		if len(fields[0]) == 64 {
			objectHash = sha256.New()
		}
		_, _ = fmt.Fprintf(objectHash, "%s %d\x00", fields[1], size)
		if err := copyCheckpointContent(ctx, io.MultiWriter(writer, objectHash), io.LimitReader(reader, size), size, ""); err != nil {
			return err
		}
		if fmt.Sprintf("%x", objectHash.Sum(nil)) != fields[0] {
			return errors.New("checkpoint Git object identity changed")
		}
		if end, err := reader.ReadByte(); err != nil || end != '\n' {
			return errors.New("checkpoint Git object was truncated")
		}
	}
	if err := errors.Join(batch.Wait(), listing.Wait()); err != nil {
		return err
	}
	// Include each new historical tree as well as the final working tree. A file
	// committed and then deleted still needs its LFS payload after failover.
	seen := filepath.Join(private, "checkpoint-lfs")
	if err := os.Mkdir(seen, 0700); err != nil {
		return err
	}
	visit := func(commit string) error {
		if !validSessionReviewObject(commit) {
			return errors.New("invalid checkpoint LFS tree")
		}
		links, err := forkspace.Gitlinks(ctx, private, commit)
		if err != nil || !maps.Equal(wantLinks, links) {
			return errors.New("nested work needs separate custody before checkpoint")
		}
		if err := command("read-tree", commit).Run(); err != nil {
			return err
		}
		return forkspace.VisitLFSPointers(ctx, private, commit, func(pointer forkspace.LFSPointer) error {
			mark, err := os.OpenFile(filepath.Join(seen, pointer.OID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if errors.Is(err, os.ErrExist) {
				previous, readErr := os.ReadFile(filepath.Join(seen, pointer.OID))
				if readErr != nil || string(previous) != strconv.FormatInt(pointer.Size, 10) {
					return errors.New("conflicting checkpoint LFS pointer sizes")
				}
				return nil
			}
			if err != nil {
				return err
			}
			_, writeErr := io.WriteString(mark, strconv.FormatInt(pointer.Size, 10))
			if err := errors.Join(writeErr, mark.Close()); err != nil {
				return err
			}
			if pointer.Size == 0 && pointer.OID == fmt.Sprintf("%x", sha256.Sum256(nil)) {
				return checkpointTarHeader(writer, "lfs/"+pointer.OID, 0644, 0)
			}
			root, err := os.OpenRoot(workspace)
			if err != nil {
				return err
			}
			defer root.Close()
			file, err := root.Open(checkpointLFSPath(pointer.OID))
			if err != nil {
				return fmt.Errorf("checkpoint needs LFS object %s: %w", pointer.OID, err)
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() != pointer.Size {
				return errors.New("checkpoint LFS object is unavailable")
			}
			if err := checkpointTarHeader(writer, "lfs/"+pointer.OID, 0644, pointer.Size); err != nil {
				return err
			}
			return copyCheckpointContent(ctx, writer, file, pointer.Size, pointer.OID)
		})
	}
	if err := visitCheckpointTrees(ctx, private, base, head, tree, visit); err != nil {
		return err
	}
	return writer.Close()
}

func visitCheckpointTrees(ctx context.Context, repository, base, head, tree string, visit func(string) error) error {
	commits, err := forkspace.GitCommandWithEnv(ctx, repository, sessionCompanionGitEnv(), "rev-list", head, "^"+base)
	if err != nil {
		return err
	}
	pipe, err := commits.StdoutPipe()
	if err != nil {
		return err
	}
	defer pipe.Close()
	if err := commits.Start(); err != nil {
		return err
	}
	defer func() { _ = commits.Process.Kill(); _ = commits.Wait() }()
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		if err := visit(scanner.Text()); err != nil {
			return err
		}
	}
	return errors.Join(scanner.Err(), commits.Wait(), visit(tree))
}

// Only object data enters the quarantine repository: no source config, refs,
// hooks, alternates, paths or executable archive metadata are ever installed.
func readCheckpointRepository(ctx context.Context, repository string, input io.Reader) error {
	reader := workerproto.NewWorkspaceCheckpointTarReader(input)
	seen := filepath.Join(repository, ".git", "checkpoint-seen")
	if err := os.Mkdir(seen, 0700); err != nil {
		return err
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		parts := strings.Split(header.Name, "/")
		if header.Format != tar.FormatGNU || header.Typeflag != tar.TypeReg || header.Mode != 0644 ||
			header.Size < 0 || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" ||
			header.Linkname != "" || !header.ModTime.Equal(time.Unix(0, 0)) || !header.AccessTime.IsZero() ||
			!header.ChangeTime.IsZero() || header.Devmajor != 0 || header.Devminor != 0 || len(header.PAXRecords) != 0 {
			return errors.New("invalid checkpoint repository header")
		}
		if len(parts) == 3 && parts[0] == "objects" && validSessionReviewObject(parts[2]) &&
			(parts[1] == "commit" || parts[1] == "tree" || parts[1] == "blob") {
			mark, err := os.OpenFile(filepath.Join(seen, parts[2]), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return errors.New("duplicate checkpoint Git object")
			}
			if err := mark.Close(); err != nil {
				return err
			}
			command, err := forkspace.GitCommandWithEnv(ctx, repository, sessionCompanionGitEnv(),
				"hash-object", "-w", "-t", parts[1], "--stdin")
			if err != nil {
				return err
			}
			output := &sessionWorkspaceLimitedWriter{limit: 128}
			command.Stdin, command.Stdout = reader, output
			if err := command.Run(); err != nil || output.truncated || strings.TrimSpace(output.buf.String()) != parts[2] {
				return errors.New("checkpoint Git object digest does not match")
			}
			continue
		}
		if len(parts) != 2 || parts[0] != "lfs" || len(parts[1]) != 64 || !validSessionReviewObject(parts[1]) {
			return errors.New("undeclared checkpoint repository member")
		}
		if err := os.WriteFile(filepath.Join(seen, "lfs-"+parts[1]), nil, 0600); err != nil {
			return err
		}
		path := filepath.Join(repository, checkpointLFSPath(parts[1]))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		err = copyCheckpointContent(ctx, file, reader, header.Size, parts[1])
		if err := errors.Join(err, file.Close()); err != nil {
			return err
		}
	}
}

func checkpointLFSPath(oid string) string {
	return filepath.Join(".git", "lfs", "objects", oid[:2], oid[2:4], oid)
}

func checkpointTarHeader(writer *tar.Writer, name string, mode, size int64) error {
	return writer.WriteHeader(&tar.Header{
		Name: name, Mode: mode, Size: size, Typeflag: tar.TypeReg,
		ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatGNU,
	})
}

// Every byte is copied, hashed and scanned with bounded memory, including binary
// files and very long lines. A partial read or a late disk error is never success.
func copyCheckpointContent(ctx context.Context, output io.Writer, input io.Reader, size int64, digest string) error {
	hash := sha256.New()
	counter := &checkpointCountWriter{writer: io.MultiWriter(output, hash)}
	found, err := secretscan.ContainsCredential(io.TeeReader(reviewScanReader{ctx, input}, counter))
	if err != nil {
		return err
	}
	if found {
		return errors.New("workspace checkpoint contains a credential")
	}
	if counter.bytes != size || (digest != "" && fmt.Sprintf("%x", hash.Sum(nil)) != digest) {
		return errors.New("workspace checkpoint content identity changed")
	}
	return nil
}

type checkpointCountWriter struct {
	writer io.Writer
	bytes  int64
}

func (w *checkpointCountWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.bytes += int64(n)
	return n, err
}
