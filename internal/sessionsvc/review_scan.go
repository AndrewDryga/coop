package sessionsvc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/hostsurface"
	"github.com/AndrewDryga/coop/internal/secretscan"
)

// Scan the actual parent-to-candidate delta, never HEAD...candidate: HEAD in
// this isolated checkout is already the candidate. No model Git driver runs.
func scanReviewCandidate(ctx context.Context, repository, parent, candidate string) ([]string, error) {
	if !validSessionReviewObject(parent) || !validSessionReviewObject(candidate) {
		return nil, errors.New("invalid review scan identity")
	}
	command, err := forkspace.GitCommandWithEnv(ctx, repository, sessionCompanionGitEnv(),
		"diff", "--raw", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv",
		"--ignore-submodules=dirty", "--submodule=short", "-z", parent, candidate, "--")
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(output, 16<<10)
	shadowed := box.NewShadowDecider(repository)
	var findings []string
	attributesChanged := false
	add := func(finding string) {
		if len(findings) < sessionReviewFindingLimit {
			findings = append(findings, SanitizeReviewText(finding, sessionReviewFindingBytes))
		}
	}
	for {
		var record []byte
		record, err = reader.ReadSlice(0)
		if errors.Is(err, io.EOF) && len(record) == 0 {
			err = nil
			break
		}
		if err != nil {
			break
		}
		fields := strings.Fields(strings.TrimSuffix(string(record), "\x00"))
		if len(fields) != 5 || !strings.HasPrefix(fields[0], ":") {
			err = errors.New("malformed review delta")
			break
		}
		record, err = reader.ReadSlice(0)
		if err != nil {
			break
		}
		path := strings.TrimSuffix(string(record), "\x00")
		attributesChanged = attributesChanged || filepath.Base(path) == ".gitattributes"
		if fields[4] == "D" {
			continue
		}
		if shadowed(path) {
			add("secret-like file: " + path)
		}
		if reason, automatic := hostsurface.Classify(fields[4], path); automatic {
			add(path + " — " + reason)
		}
		if fields[1] == "160000" {
			continue // The materializer independently refuses changed gitlinks.
		}
		if !validSessionReviewObject(fields[3]) {
			err = errors.New("invalid review blob")
			break
		}
		var content []byte
		var secret bool
		content, secret, err = scanReviewBlob(ctx, repository, fields[3])
		if err != nil {
			break
		}
		if oid, size, pointer := forkspace.ParseLFSPointer(content); pointer {
			var attributes []byte
			var truncated bool
			attributes, truncated, err = runSessionCompanionGitContext(ctx, repository, 32<<10,
				"check-attr", "--cached", "-z", "filter", "--", path)
			if err != nil || truncated {
				err = errors.Join(err, errors.New("cannot verify review LFS attributes"))
				break
			}
			if string(attributes) == path+"\x00filter\x00lfs\x00" {
				content, secret, err = scanReviewLFS(ctx, repository, oid, size)
				if err != nil {
					break
				}
			}
		}
		if secret || len(secretscan.ScanSecrets(string(content))) != 0 {
			add("possible secret in " + path + " — remove the credential before publication")
		}
		if filepath.Base(path) == "package.json" && content == nil {
			add(path + " cannot be inspected for automatic install scripts")
		} else if filepath.Base(path) == "package.json" {
			old, _, readErr := runSessionCompanionGitContext(ctx, repository, 5<<20, "show", parent+":"+path)
			if readErr != nil && fields[4] != "A" {
				err = readErr
				break
			}
			if script := addedReviewLifecycle(old, content); script != "" {
				add(path + " adds a " + script + " script — npm runs it automatically on install")
			}
		}
	}
	if err != nil {
		_ = command.Process.Kill()
	}
	err = errors.Join(err, command.Wait(), ctx.Err())
	if err == nil && attributesChanged {
		// Attributes can activate an unchanged pointer, including when a nested
		// override is deleted. In that case the pointer delta alone is insufficient.
		err = forkspace.VisitLFSPointers(ctx, repository, candidate, func(pointer forkspace.LFSPointer) error {
			content, secret, err := scanReviewLFS(ctx, repository, pointer.OID, pointer.Size)
			if secret || len(secretscan.ScanSecrets(string(content))) != 0 {
				add("possible secret in " + pointer.Path + " — remove the credential before publication")
			}
			return err
		})
	}
	return boundedSessionReviewFindings(findings), err
}

func scanReviewBlob(ctx context.Context, repository, oid string) ([]byte, bool, error) {
	command, err := forkspace.GitCommandWithEnv(ctx, repository, sessionCompanionGitEnv(), "cat-file", "blob", oid)
	if err != nil {
		return nil, false, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := command.Start(); err != nil {
		return nil, false, err
	}
	content, secret, err := scanReviewContent(ctx, output)
	if err != nil {
		_ = command.Process.Kill()
	}
	return content, secret, errors.Join(err, command.Wait(), ctx.Err())
}

func scanReviewLFS(ctx context.Context, repository, oid string, size int64) ([]byte, bool, error) {
	root, err := os.OpenRoot(repository)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	file, err := root.Open(filepath.Join(".git", "lfs", "objects", oid[:2], oid[2:4], oid))
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return nil, false, errors.New("review LFS object is unavailable")
	}
	digest := sha256.New()
	content, secret, err := scanReviewContent(ctx, io.TeeReader(file, digest))
	if err == nil && fmt.Sprintf("%x", digest.Sum(nil)) != oid {
		err = errors.New("review LFS object digest changed")
	}
	return content, secret, errors.Join(err, ctx.Err())
}

func scanReviewContent(ctx context.Context, input io.Reader) ([]byte, bool, error) {
	small := &sessionWorkspaceLimitedWriter{limit: 5 << 20}
	reader := io.TeeReader(reviewScanReader{ctx, input}, small)
	secret, scanErr := secretscan.ContainsCredential(reader)
	_, drainErr := io.Copy(io.Discard, reader)
	if small.truncated || bytes.IndexByte(small.buf.Bytes(), 0) >= 0 {
		return nil, secret, errors.Join(scanErr, drainErr)
	}
	content := small.buf.Bytes()
	if content == nil {
		content = []byte{}
	}
	return content, secret, errors.Join(scanErr, drainErr)
}

type reviewScanReader struct {
	ctx context.Context
	io.Reader
}

func (reader reviewScanReader) Read(data []byte) (int, error) {
	if err := context.Cause(reader.ctx); err != nil {
		return 0, err
	}
	return reader.Reader.Read(data)
}

func addedReviewLifecycle(before, after []byte) string {
	type manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	var old, current manifest
	_ = json.Unmarshal(before, &old)
	_ = json.Unmarshal(after, &current)
	for _, key := range []string{"preinstall", "install", "postinstall", "prepare"} {
		if current.Scripts[key] != "" && current.Scripts[key] != old.Scripts[key] {
			return key
		}
	}
	return ""
}
