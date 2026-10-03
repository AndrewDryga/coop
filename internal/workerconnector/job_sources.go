package workerconnector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

var ErrJobSourceIntegrity = errors.New("job source identity or working tree does not match")

// errJobSourceDownloading reports a repository's first download, its whole history, still
// running in the background: the command is tried again on its next delivery.
var errJobSourceDownloading = errors.New("the repository's history is still downloading")

// firstDownloadTimeout bounds a repository's first download. theblitzapp/blitz-core's 15 GB
// took about 45 minutes at 6 MiB/s (2026-10-03); a transfer that stalls is git's own to give up.
const firstDownloadTimeout = 6 * time.Hour

type firstDownload struct {
	done chan struct{}
	err  error
}

// The credential is transient host-side transport data, never part of the durable job.
// A public repository needs none: the controller grants it as Public with no token, and
// the worker fetches it anonymously, as anyone may (a repository vendoring an open-source
// library from outside every organization the controller's GitHub App reaches).
type JobSourceGrant struct {
	RepositoryRef      string    `json:"repository_ref"`
	GitHubRepository   string    `json:"github_repository"`
	GitHubRepositoryID int64     `json:"github_repository_id"`
	Token              string    `json:"token"`
	Public             bool      `json:"public,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// validCredential reports whether the grant carries a bounded credential, or says the
// repository is public and carries none.
func (g JobSourceGrant) validCredential() bool {
	if g.Public {
		return g.Token == ""
	}
	return len(g.Token) > 0 && len(g.Token) <= 4096
}

type JobSourceTransport interface {
	FetchJobSourceGrant(context.Context, string, workerproto.RepositoryIdentity) (JobSourceGrant, error)
}

type JobSourceStager interface {
	Stage(context.Context, string, workerproto.JobSource) error
}

type privateJobSourceStager struct {
	transport JobSourceTransport
	stateRoot string
	// Tests substitute a local Git remote; production always derives github.com from the job.
	remoteForTest           string
	lfsEndpointForTest      string
	submoduleRemotesForTest map[string]string
	lookupTimeoutForTest    time.Duration
	gitForTest              func(context.Context, string, string, string, ...string) (string, error)
	// A repository's first download, its whole history, runs in the background when set, as
	// NewJobSourceStager sets it: a large one takes far longer than any command may hold the
	// worker, which runs one command at a time. Later copies start from it and fetch only
	// what is new. Unset, as tests build a stager, staging waits for it.
	backgroundFirstDownload bool
	downloadsMu             sync.Mutex
	downloads               map[string]*firstDownload
}

func NewJobSourceStager(transport JobSourceTransport, stateRoot string) (*privateJobSourceStager, error) {
	if transport == nil || !filepath.IsAbs(stateRoot) {
		return nil, errors.New("job source stager needs a transport and absolute private state root")
	}
	return &privateJobSourceStager{transport: transport, stateRoot: stateRoot, backgroundFirstDownload: true}, nil
}

func (e *Executor) stageCreateJobSources(ctx context.Context, body []byte) error {
	var payload struct {
		Job json.RawMessage `json:"job"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("%w: invalid create body", ErrRequestRejected)
	}
	if len(payload.Job) == 0 {
		return nil
	}
	job, err := workerproto.DecodeJobSpec(payload.Job)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRequestRejected, err)
	}
	if job.Source == nil && len(job.Companions) == 0 {
		return nil
	}
	if e.jobSourceStager == nil {
		return fmt.Errorf("%w: job source transfer is not configured", ErrRequestRejected)
	}
	var sources []workerproto.JobSource
	if job.Source != nil {
		sources = append(sources, *job.Source)
	}
	for _, companion := range job.Companions {
		sources = append(sources, companion.Source)
	}
	// Every repository whose history is still downloading starts its download now, so a job
	// with several new repositories does not discover them one delivery at a time.
	downloading := false
	for _, source := range sources {
		if err := e.jobSourceStager.Stage(ctx, job.JobRef, source); err != nil {
			if errors.Is(err, errJobSourceDownloading) {
				downloading = true
				continue
			}
			if errors.Is(err, ErrJobSourceIntegrity) {
				return fmt.Errorf("%w: %v", ErrRequestRejected, err)
			}
			return classifyArtifactFetch(err, "fetch job source")
		}
	}
	if downloading {
		return classifyArtifactFetch(errJobSourceDownloading, "fetch job source")
	}
	return nil
}

func (s *privateJobSourceStager) Stage(ctx context.Context, jobRef string, source workerproto.JobSource) error {
	ctx, err := withSourceCredentials(ctx, s.stateRoot)
	if err != nil {
		return err
	}
	key, err := source.StagingKey()
	if err != nil {
		return err
	}
	if err := requirePrivateDirectory(s.stateRoot); err != nil {
		return err
	}
	parent := filepath.Join(s.stateRoot, "job-sources")
	if err := os.Mkdir(parent, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create private job source directory: %w", err)
	}
	if err := requirePrivateDirectory(parent); err != nil {
		return err
	}
	final := filepath.Join(parent, key)
	if _, err := os.Lstat(final); err == nil {
		if err := verifyStagedJobSource(ctx, final, source); err != nil {
			return err
		}
		return configureSourceGitIdentity(ctx, filepath.Join(final, "repository"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	seed := previousStagedRepository(parent, key, source.RepositoryIdentity())
	// The create comes back about three times a second while a history downloads, and Ryker
	// mints a GitHub token for every grant asked of it (blitz, 2026-10-03).
	if seed == "" && s.stillDownloading(parent, source.RepositoryIdentity()) {
		return errJobSourceDownloading
	}
	grant, err := s.sourceGrant(ctx, jobRef, source.RepositoryIdentity())
	if err != nil {
		return err
	}
	remote, protocol := s.remote(source.RepositoryIdentity())
	if seed == "" {
		if seed, err = s.firstDownload(ctx, parent, remote, protocol, grant.Token, source); err != nil {
			return err
		}
	}
	temporary, err := os.MkdirTemp(parent, ".source-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return err
	}
	repository := filepath.Join(temporary, "repository")
	if err := fetchVerifiedSource(ctx, repository, remote, protocol, grant.Token, source, seed); err != nil {
		if ctx.Err() != nil {
			return err
		}
		// An earlier copy is only a head start: a damaged one never stops staging. A damaged
		// first download is downloaded again, in the background like the first time.
		if seed == firstDownloadRepository(parent, source.RepositoryIdentity()) {
			if err := os.RemoveAll(filepath.Dir(seed)); err != nil {
				return err
			}
			if _, err := s.firstDownload(ctx, parent, remote, protocol, grant.Token, source); err != nil {
				return err
			}
			return errJobSourceDownloading
		}
		if err := os.RemoveAll(repository); err != nil {
			return err
		}
		if err := fetchVerifiedSource(ctx, repository, remote, protocol, grant.Token, source, ""); err != nil {
			return err
		}
	}
	if err := s.fetchLFS(ctx, filepath.Join(temporary, "repository"), source.RepositoryIdentity(), protocol, grant.Token,
		source.Binding.SelectedCommit, source.Binding.DefaultCommit); err != nil {
		return err
	}
	if err := s.stageSubmodules(ctx, jobRef, filepath.Join(temporary, "repository"), source.Binding.SelectedCommit, source.Submodules); err != nil {
		return err
	}
	if err := verifySourceRepository(ctx, filepath.Join(temporary, "repository"), source); err != nil {
		return err
	}
	document, err := json.Marshal(source)
	if err != nil {
		return err
	}
	receipt, err := os.OpenFile(filepath.Join(temporary, "source.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := receipt.Write(document)
	syncErr := receipt.Sync()
	closeErr := receipt.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(temporary, final); err != nil {
		if _, statErr := os.Lstat(final); statErr == nil {
			return verifyStagedJobSource(ctx, final, source)
		}
		return fmt.Errorf("publish private job source: %w", err)
	}
	return nil
}

// firstDownloadRepository is where a repository's first download keeps its whole history. Its
// name is a digest like a staged source's, so the storage inventory accepts it, and its
// source.json names the repository, so later stagings take it as their earlier copy.
func firstDownloadRepository(parent string, identity workerproto.RepositoryIdentity) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("coop-first-download\x00%s\x00%s\x00%d",
		identity.RepositoryRef, identity.GitHubRepository, identity.GitHubRepositoryID)))
	return filepath.Join(parent, hex.EncodeToString(sum[:]), "repository")
}

// firstDownload returns the repository's whole history for a staging to start from, and
// downloads it the first time. In the background (backgroundFirstDownload) it reports
// errJobSourceDownloading until the download is there; otherwise it waits for it.
func (s *privateJobSourceStager) firstDownload(ctx context.Context, parent, remote, protocol, token string, source workerproto.JobSource) (string, error) {
	repository := firstDownloadRepository(parent, source.RepositoryIdentity())
	final := filepath.Dir(repository)
	if _, err := os.Lstat(filepath.Join(final, "source.json")); err == nil {
		return repository, nil
	}
	if !s.backgroundFirstDownload {
		if err := s.downloadHistory(ctx, parent, final, remote, protocol, token, source); err != nil {
			return "", err
		}
		return repository, nil
	}
	s.downloadsMu.Lock()
	defer s.downloadsMu.Unlock()
	if s.downloads == nil {
		s.downloads = map[string]*firstDownload{}
	}
	if download := s.downloads[final]; download != nil {
		select {
		case <-download.done:
			delete(s.downloads, final)
			if download.err != nil {
				return "", download.err
			}
			return repository, nil
		default:
			return "", errJobSourceDownloading
		}
	}
	download := &firstDownload{done: make(chan struct{})}
	s.downloads[final] = download
	go func() {
		defer close(download.done)
		background, cancel := context.WithTimeout(context.Background(), firstDownloadTimeout)
		defer cancel()
		download.err = s.downloadHistory(background, parent, final, remote, protocol, token, source)
	}()
	return "", errJobSourceDownloading
}

// stillDownloading reports a first download of the repository under way in the background. One
// that has finished is not: firstDownload collects its outcome.
func (s *privateJobSourceStager) stillDownloading(parent string, source workerproto.RepositoryIdentity) bool {
	if !s.backgroundFirstDownload {
		return false
	}
	s.downloadsMu.Lock()
	defer s.downloadsMu.Unlock()
	download := s.downloads[filepath.Dir(firstDownloadRepository(parent, source))]
	if download == nil {
		return false
	}
	select {
	case <-download.done:
		return false
	default:
		return true
	}
}

// downloadHistory fetches the repository's default branch with its whole history into a
// scratch directory and publishes it at final. Progress keeps bytes flowing while GitHub packs
// a large history, minutes before its first object: a quiet fetch receives nothing then, and
// git's low-speed limit gives up on it.
func (s *privateJobSourceStager) downloadHistory(ctx context.Context, parent, final, remote, protocol, token string, source workerproto.JobSource) error {
	ctx, err := withSourceCredentials(ctx, s.stateRoot)
	if err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(parent, ".source-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return err
	}
	repository := filepath.Join(temporary, "repository")
	if err := runSourceGit(ctx, "", token, protocol, "init", "--quiet", "--template=", "--object-format=sha1", repository); err != nil {
		return err
	}
	if err := os.Chmod(repository, 0o700); err != nil {
		return err
	}
	if err := runSourceGit(ctx, repository, token, protocol, "remote", "add", "origin", remote); err != nil {
		return err
	}
	if err := runSourceGit(ctx, repository, token, protocol, "fetch", "--progress", "--no-tags", "origin",
		"+"+source.Binding.DefaultRef+":refs/remotes/origin/job-default"); err != nil {
		return err
	}
	document, err := json.Marshal(source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(temporary, "source.json"), document, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, final); err != nil {
		if _, statErr := os.Lstat(filepath.Join(final, "source.json")); statErr == nil {
			return nil
		}
		return fmt.Errorf("publish first download: %w", err)
	}
	return nil
}

func (s *privateJobSourceStager) remote(source workerproto.RepositoryIdentity) (string, string) {
	if remote := s.submoduleRemotesForTest[source.GitHubRepository]; remote != "" {
		return remote, "file"
	}
	if s.remoteForTest != "" {
		return s.remoteForTest, "file"
	}
	return "https://github.com/" + source.GitHubRepository + ".git", "https"
}

func (s *privateJobSourceStager) sourceGrant(ctx context.Context, jobRef string, source workerproto.RepositoryIdentity) (JobSourceGrant, error) {
	grant, err := s.transport.FetchJobSourceGrant(ctx, jobRef, source)
	if err != nil {
		return JobSourceGrant{}, err
	}
	if grant.RepositoryRef != source.RepositoryRef || grant.GitHubRepository != source.GitHubRepository ||
		grant.GitHubRepositoryID != source.GitHubRepositoryID || !grant.validCredential() ||
		!grant.ExpiresAt.After(time.Now().Add(30*time.Second)) {
		return JobSourceGrant{}, ErrJobSourceIntegrity
	}
	return grant, nil
}

// RefreshDefault proves the current default without rewriting the frozen source. Each call
// obtains current job authority: review checks again after its gate to detect a moving parent.
func (s *privateJobSourceStager) RefreshDefault(ctx context.Context, jobRef string, source workerproto.JobSource, repository string) (string, error) {
	ctx, err := withSourceCredentials(ctx, s.stateRoot)
	if err != nil {
		return "", err
	}
	key, err := source.StagingKey()
	if err != nil {
		return "", err
	}
	parent := filepath.Join(s.stateRoot, "job-sources")
	directory := filepath.Join(parent, key)
	if repository != filepath.Join(directory, "repository") {
		return "", ErrJobSourceIntegrity
	}
	for _, path := range []string{s.stateRoot, parent} {
		if err := requirePrivateDirectory(path); err != nil {
			return "", err
		}
	}
	if err := verifyStagedJobSource(ctx, directory, source); err != nil {
		return "", err
	}
	grant, err := s.sourceGrant(ctx, jobRef, source.RepositoryIdentity())
	if err != nil {
		return "", err
	}
	run, lookupTimeout := sourceGitValue, 30*time.Second
	if s.gitForTest != nil {
		run = s.gitForTest
	}
	if s.lookupTimeoutForTest != 0 {
		lookupTimeout = s.lookupTimeoutForTest
	}
	remote, protocol := s.remote(source.RepositoryIdentity())
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	resolved, err := run(lookupCtx, repository, grant.Token, protocol, "ls-remote", "--exit-code", "--refs", "--", remote, source.Binding.DefaultRef)
	cancel()
	if err != nil {
		return "", err
	}
	fields := strings.Split(resolved, "\t")
	if len(fields) != 2 || fields[1] != source.Binding.DefaultRef {
		return "", ErrJobSourceIntegrity
	}
	head := fields[0]
	decoded, err := hex.DecodeString(head)
	if err != nil || len(decoded) != 20 || hex.EncodeToString(decoded) != head {
		return "", ErrJobSourceIntegrity
	}
	if _, err := run(ctx, repository, "", "", "cat-file", "-e", head+"^{commit}"); err != nil {
		// The lookup's budget is over. A progressing transfer has no total-time cap;
		// HTTP low-speed bounds below abort stalls, and the owning operation can cancel it.
		if _, err := run(ctx, repository, grant.Token, protocol, "fetch", "--quiet", "--no-write-fetch-head", "--no-tags", "--", remote, head); err != nil {
			return "", err
		}
	}
	if actual, err := run(ctx, repository, "", "", "rev-parse", head+"^{commit}"); err != nil || actual != head {
		return "", ErrJobSourceIntegrity
	}
	if err := s.fetchLFS(ctx, repository, source.RepositoryIdentity(), protocol, grant.Token, source.Binding.SelectedCommit, head); err != nil {
		return "", err
	}
	if err := verifyStagedJobSource(ctx, directory, source); err != nil {
		return "", err
	}
	return head, nil
}

func fetchVerifiedSource(ctx context.Context, destination, remote, protocol, token string, source workerproto.JobSource, seed string) error {
	if err := runSourceGit(ctx, "", token, protocol, "init", "--quiet", "--template=", "--object-format=sha1", destination); err != nil {
		return err
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return err
	}
	if err := configureSourceGitIdentity(ctx, destination); err != nil {
		return err
	}
	if err := runSourceGit(ctx, destination, token, protocol, "remote", "add", "origin", remote); err != nil {
		return err
	}
	if seed != "" {
		if err := seedFromStagedCopy(ctx, destination, seed); err != nil {
			return err
		}
	}
	binding := source.Binding
	if err := runSourceGit(ctx, destination, token, protocol, "fetch", "--quiet", "--no-tags", "origin", "+"+binding.DefaultRef+":refs/remotes/origin/job-default"); err != nil {
		return err
	}
	if actual, err := sourceGitValue(ctx, destination, token, protocol, "rev-parse", "refs/remotes/origin/job-default^{commit}"); err != nil || actual != binding.DefaultCommit {
		return ErrJobSourceIntegrity
	}
	if selected := binding.SelectedRefValue(); selected != "" && selected != binding.DefaultRef {
		if err := runSourceGit(ctx, destination, token, protocol, "fetch", "--quiet", "--no-tags", "origin", "+"+selected+":refs/remotes/origin/job-selected"); err != nil {
			return err
		}
		if actual, err := sourceGitValue(ctx, destination, token, protocol, "rev-parse", "refs/remotes/origin/job-selected^{commit}"); err != nil || actual != binding.SelectedCommit {
			return ErrJobSourceIntegrity
		}
	} else if selected == "" {
		if err := runSourceGit(ctx, destination, token, protocol, "fetch", "--quiet", "--no-tags", "origin", binding.SelectedCommit); err != nil {
			return err
		}
	}
	if seed != "" {
		if err := runSourceGit(ctx, destination, "", protocol, "update-ref", "-d", stagedSeedRef); err != nil {
			return err
		}
	}
	if actual, err := sourceGitValue(ctx, destination, token, protocol, "merge-base", binding.DefaultCommit, binding.SelectedCommit); err != nil || actual != binding.BaseCommit {
		return ErrJobSourceIntegrity
	}
	if actual, err := sourceGitValue(ctx, destination, token, protocol, "rev-parse", binding.SelectedCommit+"^{tree}"); err != nil || actual != binding.AdmittedTree {
		return ErrJobSourceIntegrity
	}
	if err := runSourceGit(ctx, destination, token, protocol, "checkout", "--quiet", "--detach", binding.SelectedCommit); err != nil {
		return err
	}
	return verifySourceHead(ctx, destination, binding.SelectedCommit, binding.AdmittedTree)
}

// stagedSeedRef names an earlier copy's default commit while a new copy fetches, so Git asks
// the remote only for what that commit does not already have. It is gone before verification.
const stagedSeedRef = "refs/coop/staged-seed"

// previousStagedRepository is the newest staged copy of the same repository: emisar's default
// branch moved on 2026-10-01 and every new session cloned its 93,414 objects again at 93 KB/s,
// past the command's lease, from scratch on each retry, while every other worker command,
// routing's too, waited behind it. Only a copy whose receipt names this exact repository is
// used, and only as a head start: the new copy is verified on its own like any other.
func previousStagedRepository(parent, key string, identity workerproto.RepositoryIdentity) string {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return ""
	}
	var newest string
	var newestAt time.Time
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") || name == key {
			continue
		}
		document, err := os.ReadFile(filepath.Join(parent, name, "source.json"))
		if err != nil || len(document) > 256<<10 {
			continue
		}
		var recorded workerproto.JobSource
		if json.Unmarshal(document, &recorded) != nil || recorded.RepositoryIdentity() != identity {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = filepath.Join(parent, name, "repository"), info.ModTime()
		}
	}
	return newest
}

// seedFromStagedCopy links an earlier copy's objects into a new repository, packs and loose
// objects alike: an object never changes, so both copies can share its file. A ref to the
// earlier default commit then lets the fetch ask the remote only for what is new.
func seedFromStagedCopy(ctx context.Context, destination, seed string) error {
	commit, err := sourceGitValue(ctx, seed, "", "https", "rev-parse", "--verify", "refs/remotes/origin/job-default^{commit}")
	if err != nil {
		return err
	}
	from := filepath.Join(seed, ".git", "objects")
	to := filepath.Join(destination, ".git", "objects")
	err = filepath.WalkDir(from, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if relative == "info" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(to, relative), 0o700)
		}
		if !entry.Type().IsRegular() || strings.HasSuffix(relative, ".keep") {
			return nil
		}
		target := filepath.Join(to, relative)
		if _, err := os.Lstat(target); err == nil {
			return nil
		}
		return os.Link(path, target)
	})
	if err != nil {
		return err
	}
	return runSourceGit(ctx, destination, "", "https", "update-ref", stagedSeedRef, commit)
}

func (s *privateJobSourceStager) stageSubmodules(ctx context.Context, jobRef, repository, commit string, modules []workerproto.JobSubmodule) error {
	if err := verifySourceGitlinks(ctx, repository, commit, modules); err != nil {
		return err
	}
	for _, module := range modules {
		directory, err := forkspace.SubmoduleDirectory(repository, module.Path)
		if err != nil {
			return ErrJobSourceIntegrity
		}
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 0 {
			return ErrJobSourceIntegrity
		}
		grant, err := s.sourceGrant(ctx, jobRef, module.RepositoryIdentity())
		if err != nil {
			return err
		}
		remote, protocol := s.remote(module.RepositoryIdentity())
		if err := runSourceGit(ctx, "", "", protocol, "init", "--quiet", "--template=", "--object-format=sha1", directory); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return err
		}
		if err := configureSourceGitIdentity(ctx, directory); err != nil {
			return err
		}
		if err := runSourceGit(ctx, directory, "", protocol, "remote", "add", "origin", remote); err != nil {
			return err
		}
		if err := runSourceGit(ctx, directory, grant.Token, protocol, "fetch", "--quiet", "--no-tags", "origin", module.Commit); err != nil {
			return err
		}
		if err := runSourceGit(ctx, directory, "", "", "checkout", "--quiet", "--detach", module.Commit); err != nil {
			return err
		}
		if err := verifySourceHead(ctx, directory, module.Commit, module.Tree); err != nil {
			return err
		}
		if err := s.fetchLFS(ctx, directory, module.RepositoryIdentity(), protocol, grant.Token, module.Commit); err != nil {
			return err
		}
		if err := s.stageSubmodules(ctx, jobRef, directory, module.Commit, module.Submodules); err != nil {
			return err
		}
	}
	return nil
}

func (s *privateJobSourceStager) fetchLFS(ctx context.Context, repository string, identity workerproto.RepositoryIdentity, protocol, token string, commits ...string) error {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		return errors.New("git-lfs is required on the worker to fetch complete working trees")
	}
	endpoint := "https://github.com/" + identity.GitHubRepository + ".git/info/lfs"
	if s.lfsEndpointForTest != "" {
		endpoint = s.lfsEndpointForTest
	}
	args := []string{
		"-c", "lfs.url=" + endpoint, "-c", "remote.origin.lfsurl=" + endpoint,
		"-c", "lfs.basictransfersonly=true", "-c", "lfs.standalonetransferagent=",
		"-c", "lfs.remote.autodetect=false", "-c", "lfs.remote.searchall=false",
		"-c", "lfs.transfer.enablehrefrewrite=false", "-c", "lfs.skipdownloaderrors=false",
		"-c", "lfs.fetchrecentalways=false", "lfs", "fetch", "-I", "", "-X", "", "origin",
	}
	refs := slices.Clone(commits)
	slices.Sort(refs)
	args = append(args, slices.Compact(refs)...)
	if err := runSourceGit(ctx, repository, token, protocol, args...); err != nil {
		return errors.New("job source LFS transfer failed")
	}
	return forkspace.HydrateLFS(ctx, repository, commits[0])
}

func verifySourceGitlinks(ctx context.Context, repository, commit string, modules []workerproto.JobSubmodule) error {
	links, err := forkspace.Gitlinks(ctx, repository, commit)
	if err != nil || len(links) != len(modules) {
		return ErrJobSourceIntegrity
	}
	for _, module := range modules {
		if links[module.Path] != module.Commit {
			return ErrJobSourceIntegrity
		}
	}
	return nil
}

func configureSourceGitIdentity(ctx context.Context, repository string) error {
	// Automated work must not depend on (or impersonate) the worker operator's
	// global Git identity. Forks and review scratch inherit these explicit values.
	for _, setting := range [][2]string{{"user.name", "Coop"}, {"user.email", "coop@localhost"}} {
		if err := runSourceGit(ctx, repository, "", "", "config", setting[0], setting[1]); err != nil {
			return err
		}
	}
	return nil
}

func runSourceGit(ctx context.Context, directory, token, protocol string, args ...string) error {
	_, err := sourceGitValue(ctx, directory, token, protocol, args...)
	return err
}

func sourceGitValue(ctx context.Context, directory, token, protocol string, args ...string) (string, error) {
	output, err := sourceGitBytes(ctx, directory, token, protocol, args...)
	return strings.TrimSpace(string(output)), err
}

func sourceGitBytes(ctx context.Context, directory, token, protocol string, args ...string) ([]byte, error) {
	var output sourceGitOutput
	err := sourceGitIO(ctx, directory, token, protocol, nil, &output, args...)
	if output.exceeded {
		return nil, ErrJobSourceIntegrity
	}
	return output.buf.Bytes(), err
}

func sourceGitIO(ctx context.Context, directory, token, protocol string, input io.Reader, output io.Writer, args ...string) (returnErr error) {
	command := sourceGitCommand(ctx, directory, protocol, args...)
	if token != "" {
		// Git LFS includes every GIT_* environment value in diagnostic logs.
		// A transient owner-only config outside all checkouts keeps credentials
		// out of those logs as well as argv, repository config and model mounts.
		directory, _ := ctx.Value(sourceCredentialDirectory{}).(string)
		if directory == "" {
			return errors.New("authenticated host Git requires private credential storage")
		}
		if err := requirePrivateDirectory(directory); err != nil {
			return err
		}
		credential, err := os.CreateTemp(directory, "git-")
		if err != nil {
			return err
		}
		defer func() { returnErr = errors.Join(returnErr, os.Remove(credential.Name())) }()
		header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		_, writeErr := fmt.Fprintf(credential, "[http \"https://github.com/\"]\nextraHeader = Authorization: Basic %s\n", header)
		if err := errors.Join(writeErr, credential.Close()); err != nil {
			return err
		}
		for i, value := range command.Env {
			if strings.HasPrefix(value, "GIT_CONFIG_GLOBAL=") {
				command.Env[i] = "GIT_CONFIG_GLOBAL=" + credential.Name()
			}
		}
	}
	command.Stdin, command.Stdout = input, output
	err := command.Run()
	if errors.Is(err, exec.ErrWaitDelay) && command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("host Git operation failed")
	}
	return nil
}

type sourceGitOutput struct {
	buf      bytes.Buffer
	exceeded bool
}

func (b *sourceGitOutput) Len() int { return b.buf.Len() }

func (b *sourceGitOutput) Write(data []byte) (int, error) {
	if len(data) > 4096-b.Len() {
		b.exceeded = true
		return 0, ErrJobSourceIntegrity
	}
	return b.buf.Write(data)
}

func sourceGitCommand(ctx context.Context, directory, protocol string, args ...string) *exec.Cmd {
	args = append(append([]string{}, forkspace.GitHardening...), append([]string{"-c", "submodule.recurse=false", "-c", "fetch.recurseSubmodules=false"}, args...)...)
	if directory != "" {
		args = append([]string{"-C", directory}, args...)
	}
	command := exec.CommandContext(ctx, "git", args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = time.Second
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TEMPLATE_DIR=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_ALLOW_PROTOCOL=" + protocol, "GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=http.lowSpeedLimit", "GIT_CONFIG_VALUE_1=1",
		"GIT_CONFIG_KEY_2=http.lowSpeedTime", "GIT_CONFIG_VALUE_2=120",
	}
	return command
}

func requirePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("job source directory is not private")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("job source directory has another owner")
	}
	return nil
}

func verifyStagedJobSource(ctx context.Context, directory string, source workerproto.JobSource) error {
	if err := requirePrivateDirectory(directory); err != nil {
		return err
	}
	receiptPath := filepath.Join(directory, "source.json")
	info, err := os.Lstat(receiptPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 256<<10 {
		return ErrJobSourceIntegrity
	}
	document, err := os.ReadFile(receiptPath)
	if err != nil || len(document) > 256<<10 {
		return ErrJobSourceIntegrity
	}
	var recorded workerproto.JobSource
	if err := json.Unmarshal(document, &recorded); err != nil {
		return ErrJobSourceIntegrity
	}
	expected, _ := json.Marshal(source)
	actual, _ := json.Marshal(recorded)
	if string(actual) != string(expected) {
		return ErrJobSourceIntegrity
	}
	return verifySourceRepository(ctx, filepath.Join(directory, "repository"), source)
}

func verifySourceRepository(ctx context.Context, repository string, source workerproto.JobSource) error {
	return verifySourceTree(ctx, repository, source.Binding.SelectedCommit, source.Binding.AdmittedTree, source.Submodules)
}

func verifySourceTree(ctx context.Context, repository, commit, tree string, modules []workerproto.JobSubmodule) error {
	if err := verifySourceHead(ctx, repository, commit, tree); err != nil {
		return err
	}
	if err := verifySourceGitlinks(ctx, repository, commit, modules); err != nil {
		return err
	}
	if err := forkspace.VerifyLFS(ctx, repository, commit); err != nil {
		return ErrJobSourceIntegrity
	}
	for _, module := range modules {
		directory, err := forkspace.SubmoduleDirectory(repository, module.Path)
		if err != nil {
			return ErrJobSourceIntegrity
		}
		if err := verifySourceTree(ctx, directory, module.Commit, module.Tree, module.Submodules); err != nil {
			return err
		}
	}
	return nil
}

func verifySourceHead(ctx context.Context, repository, commit, tree string) error {
	if err := requirePrivateDirectory(repository); err != nil {
		return ErrJobSourceIntegrity
	}
	for _, check := range []struct {
		args     []string
		expected string
	}{
		{[]string{"rev-parse", "HEAD"}, commit},
		{[]string{"rev-parse", "HEAD^{tree}"}, tree},
	} {
		actual, err := sourceGitValue(ctx, repository, "", "https", check.args...)
		if err != nil || actual != check.expected {
			return ErrJobSourceIntegrity
		}
	}
	command := sourceGitCommand(ctx, repository, "", "-c", "core.attributesFile=/dev/null",
		"status", "--porcelain=v2", "--untracked-files=all", "--no-renames", "--ignore-submodules=all", "-z")
	var status sourceGitOutput
	err := forkspace.RunLFSStatus(ctx, repository, command, &status, func(limit int, args ...string) ([]byte, error) {
		output, err := sourceGitBytes(ctx, repository, "", "", args...)
		if len(output) > limit {
			return nil, ErrJobSourceIntegrity
		}
		return output, err
	})
	if err != nil || status.Len() != 0 {
		return ErrJobSourceIntegrity
	}
	return nil
}
