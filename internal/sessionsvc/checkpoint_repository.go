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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/secretscan"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Raw typed objects, not a compressed Git pack or binary patch: the controller's
// streaming credential scan must see even content later deleted from history.
// upstream, when set, is a commit of the job's authorized source (see checkpointUpstream) that
// restore fetches on its own, so its objects stay out of the transfer like the base's.
func writeCheckpointRepository(ctx context.Context, workspace, base, upstream, head, tree string, env []string, output io.Writer) error {
	command := func(args ...string) *exec.Cmd {
		cmd := forkspace.TrustedGitCommand(ctx, workspace, env, args...)
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
	if err != nil {
		return err
	}
	if !maps.Equal(wantLinks, links) {
		return checkpointRefusal("nested work needs separate custody before checkpoint")
	}
	writer := tar.NewWriter(output)
	// An explicit positive tree bypasses a commit-only exclusion. Restore fetches
	// the authorized base independently, so subtract its tree closure too.
	exclude := []string{"^" + base, "^" + base + "^{tree}"}
	if upstream != "" {
		exclude = append(exclude, "^"+upstream, "^"+upstream+"^{tree}")
	}
	listing := command(append([]string{"rev-list", "--objects", "--no-object-names", head, tree}, exclude...)...)
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
		if len(fields) != 3 || !forkspace.ValidObjectID(fields[0]) ||
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
		if !forkspace.ValidObjectID(commit) {
			return errors.New("invalid checkpoint LFS tree")
		}
		links, err := forkspace.Gitlinks(ctx, private, commit)
		if err != nil {
			return err
		}
		if !maps.Equal(wantLinks, links) {
			return checkpointRefusal("nested work needs separate custody before checkpoint")
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
				return checkpointRefusal("checkpoint needs LFS object " + pointer.OID)
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() != pointer.Size {
				return checkpointRefusal("checkpoint LFS object " + pointer.OID + " is unavailable")
			}
			if err := checkpointTarHeader(writer, "lfs/"+pointer.OID, 0644, pointer.Size); err != nil {
				return err
			}
			err = copyCheckpointContent(ctx, writer, file, pointer.Size, pointer.OID)
			if checkpointCredentialRefusal(err) {
				// LFS payloads always travel, upstream ones too, so the answer says which kind it was.
				return &session.Error{Code: session.CodeInvalidSessionState, Detail: checkpointLFSCredentialDetail}
			}
			return err
		})
	}
	if err := visitCheckpointTrees(ctx, private, base, head, tree, visit); err != nil {
		return err
	}
	return writer.Close()
}

// checkpointUpstream names the newest commit the workspace history shares with the job's
// authorized default head, which the worker itself just refreshed into its private source copy.
// It returns "" when that history adds nothing past the base. Only the private copy proves the
// answer: an agent can write any ref, config or object in the workspace, but cannot make a commit
// an ancestor of a default head the worker fetched from the controller-granted source.
func checkpointUpstream(ctx context.Context, workspace, source, base, head, defaultHead string, env []string) (string, error) {
	if !forkspace.ValidObjectID(defaultHead) {
		return "", errors.New("invalid checkpoint upstream head")
	}
	common, err := sessionCompanionGitCommonDirContext(ctx, source)
	if err != nil {
		return "", err
	}
	// The snapshot store already reads the workspace's objects; the source's join them read-only
	// for this one command, so a single merge-base sees both histories.
	merge := forkspace.TrustedGitCommand(ctx, workspace,
		append(slices.Clip(env), "GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(common, "objects")),
		"merge-base", head, defaultHead)
	output, err := merge.Output()
	upstream := strings.TrimSpace(string(output))
	if err != nil || !forkspace.ValidObjectID(upstream) || upstream == base {
		return "", nil // no shared history past the base to leave out
	}
	for _, pair := range [][2]string{{base, upstream}, {upstream, defaultHead}} {
		check, err := forkspace.GitCommandWithEnv(ctx, source, sessionCompanionGitEnv(), "merge-base", "--is-ancestor", pair[0], pair[1])
		if err != nil {
			return "", err
		}
		if check.Run() != nil {
			return "", nil
		}
	}
	return upstream, nil
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
func readCheckpointRepository(ctx context.Context, repository string, input io.Reader) (err error) {
	reader := workerproto.NewWorkspaceCheckpointTarReader(input)
	seen := filepath.Join(repository, ".git", "checkpoint-seen")
	if err := os.Mkdir(seen, 0700); err != nil {
		return err
	}
	objects := &checkpointObjectWriters{ctx: ctx, repository: repository}
	defer func() { err = objects.finish(err) }()
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
		if len(parts) == 3 && parts[0] == "objects" && forkspace.ValidObjectID(parts[2]) &&
			(parts[1] == "commit" || parts[1] == "tree" || parts[1] == "blob") {
			// The duplicate mark also stages the member's bytes for its type's one hash-object.
			mark, err := os.OpenFile(filepath.Join(seen, parts[2]), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return errors.New("duplicate checkpoint Git object")
			}
			_, err = io.Copy(mark, reviewScanReader{ctx, reader})
			if err := errors.Join(err, mark.Close()); err != nil {
				return err
			}
			if err := objects.write(parts[1], parts[2], filepath.Join(seen, parts[2])); err != nil {
				return err
			}
			continue
		}
		if len(parts) != 2 || parts[0] != "lfs" || len(parts[1]) != 64 || !forkspace.ValidObjectID(parts[1]) {
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

// checkpointObjectWriters writes staged objects through one `git hash-object --stdin-paths`
// per object type, not one process per object, and checks every printed id, in order, against the
// member's name. A checkpoint of thousands of objects used to start thousands of processes while
// its session was fenced.
type checkpointObjectWriters struct {
	ctx        context.Context
	repository string
	writers    map[string]*checkpointObjectWriter
	staged     []string
}

type checkpointObjectWriter struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	output  *sessionWorkspaceLimitedWriter
	want    []string
}

func (w *checkpointObjectWriters) write(kind, oid, path string) error {
	if strings.ContainsAny(path, "\n\r") {
		return errors.New("checkpoint staging path is not a single line")
	}
	writer := w.writers[kind]
	if writer == nil {
		command, err := forkspace.GitCommandWithEnv(w.ctx, w.repository, sessionCompanionGitEnv(),
			"hash-object", "-w", "--no-filters", "-t", kind, "--stdin-paths")
		if err != nil {
			return err
		}
		stdin, err := command.StdinPipe()
		if err != nil {
			return err
		}
		writer = &checkpointObjectWriter{command: command, stdin: stdin, output: &sessionWorkspaceLimitedWriter{limit: 1 << 30}}
		command.Stdout = writer.output
		if err := command.Start(); err != nil {
			return err
		}
		if w.writers == nil {
			w.writers = map[string]*checkpointObjectWriter{}
		}
		w.writers[kind] = writer
	}
	writer.want = append(writer.want, oid)
	w.staged = append(w.staged, path)
	if _, err := io.WriteString(writer.stdin, path+"\n"); err != nil {
		return errors.New("checkpoint Git object digest does not match") // git refused an earlier object
	}
	return nil
}

// finish waits for every writer, then empties the staged copies: the marks stay for the
// duplicate and LFS checks, and the bytes now live in the object store.
func (w *checkpointObjectWriters) finish(err error) error {
	for _, writer := range w.writers {
		if err != nil {
			_ = writer.command.Process.Kill()
		}
		waitErr := errors.Join(writer.stdin.Close(), writer.command.Wait())
		if err == nil && (waitErr != nil || writer.output.truncated || strings.Join(writer.want, "\n")+"\n" != writer.output.buf.String()) {
			err = errors.New("checkpoint Git object digest does not match")
		}
	}
	for _, path := range w.staged {
		err = errors.Join(err, os.Truncate(path, 0))
	}
	return err
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

// The refusal details never repeat what matched. Each says only where it was found.
const (
	checkpointCredentialDetail    = "workspace checkpoint contains a credential"
	checkpointLFSCredentialDetail = checkpointCredentialDetail + " in an LFS file"
	// Capture checked the job's default branch, and the matched Git content is in neither it nor the
	// base: the task added it, or it came from another branch, a pull request or a pinned commit.
	checkpointTaskCredentialDetail = checkpointCredentialDetail + " that neither the session base nor the job's default branch holds"
)

// errCheckpointNoUpstream says the job's default shares no history with the work past its base.
var errCheckpointNoUpstream = errors.New("checkpoint has no authorized upstream to leave out")

func checkpointCredentialRefusal(err error) bool {
	var refusal *session.Error
	return errors.As(err, &refusal) && strings.HasPrefix(refusal.Detail, checkpointCredentialDetail)
}

// checkpointUpstreamUnverified answers a credential refusal that a refresh of the job's source
// might have lifted but could not run: retryable when the source was unreachable, final when the
// controller refuses access or the session's job authority no longer holds.
func checkpointUpstreamUnverified(refreshErr error) error {
	var refusal *session.Error
	if errors.As(refreshErr, &refusal) && refusal.Code == session.CodeInvalidSessionState {
		return &session.Error{Code: session.CodeInvalidSessionState, Detail: checkpointCredentialDetail +
			" past the session base, and the job's default branch cannot be checked for it: " + refusal.Detail}
	}
	return &session.Error{Code: session.CodeRepositoryUnavailable, Detail: checkpointCredentialDetail +
		" past the session base, and the job's default branch could not be refreshed to check whether it came from there; retry with a new idempotency key"}
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
		return &session.Error{Code: session.CodeInvalidSessionState, Detail: checkpointCredentialDetail}
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
