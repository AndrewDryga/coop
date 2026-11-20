package workerconnector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// missingSourceFixture is a remote with one commit and a source on it, a commit or branch the
// remote may not have.
func missingSourceFixture(t *testing.T, kind session.SourceKind) (string, workerproto.JobSource) {
	t.Helper()
	remote, source := backgroundStagingFixture(t)
	return remote, missingSource(source, kind)
}

// missingSource names, instead of the source's commit, one the remote lacks, or a branch it lacks.
func missingSource(source workerproto.JobSource, kind session.SourceKind) workerproto.JobSource {
	missing := strings.Repeat("e", 40)
	source.Binding.Kind, source.Binding.SelectedCommit = kind, missing
	switch kind {
	case session.SourceCommit:
		source.Binding.SelectedRef, source.Binding.Requested = nil, session.SourceSelector{Kind: session.SourceCommit, SHA: missing}
	case session.SourceBranch:
		ref := "refs/heads/gone"
		source.Binding.SelectedRef, source.Binding.Requested = &ref, session.SourceSelector{Kind: session.SourceBranch, Name: "gone"}
	}
	return source
}

func privateStagerRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// A commit or ref the remote answers as missing refuses the job after missingSourceTries tries,
// each with one grant, so the controller gets a result and stops minting tokens.
func TestAJobSourceMissingOnTheRemoteIsRefusedAfterAFewTries(t *testing.T) {
	for _, kind := range []session.SourceKind{session.SourceCommit, session.SourceBranch} {
		t.Run(string(kind), func(t *testing.T) {
			remote, source := missingSourceFixture(t, kind)
			grants := &jobSourceGrantFixture{}
			executor := &Executor{jobSourceStager: &privateJobSourceStager{transport: grants, stateRoot: privateStagerRoot(t), remoteForTest: remote}}
			for try := 1; try <= missingSourceTries; try++ {
				_, _, err := executor.stageJobSources(context.Background(), "job:missing", []workerproto.JobSource{source})
				if try < missingSourceTries {
					if !errors.Is(err, errArtifactTransfer) || errors.Is(err, ErrRequestRejected) {
						t.Fatalf("try %d = %v, want it tried again", try, err)
					}
					continue
				}
				if !errors.Is(err, ErrRequestRejected) || !strings.Contains(err.Error(), errJobSourceMissing.Error()) {
					t.Fatalf("try %d = %v, want the job refused as missing", try, err)
				}
			}
			if grants.calls != missingSourceTries {
				t.Fatalf("grants = %d, want %d", grants.calls, missingSourceTries)
			}
		})
	}
}

// As the worker stages, downloads and copies in the background, a missing commit is refused too,
// after one grant for the history download and one per try.
func TestAJobSourceMissingOnTheRemoteIsRefusedWhenStagedInTheBackground(t *testing.T) {
	remote, source := missingSourceFixture(t, session.SourceCommit)
	grants := &jobSourceGrantFixture{}
	executor := &Executor{jobSourceStager: &privateJobSourceStager{transport: grants, stateRoot: privateStagerRoot(t), remoteForTest: remote,
		backgroundFirstDownload: true, backgroundStaging: true}}
	deadline := time.Now().Add(30 * time.Second)
	for {
		pending, _, err := executor.stageJobSources(context.Background(), "job:missing", []workerproto.JobSource{source})
		if errors.Is(err, ErrRequestRejected) {
			if !strings.Contains(err.Error(), errJobSourceMissing.Error()) {
				t.Fatalf("refused as %v, want missing", err)
			}
			break
		}
		if err == nil && len(pending) == 0 {
			t.Fatal("a missing commit was staged")
		}
		if err != nil && !errors.Is(err, errArtifactTransfer) {
			t.Fatalf("staging a missing commit = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("a missing commit is still tried again")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grants.calls != missingSourceTries+1 {
		t.Fatalf("grants = %d, want %d", grants.calls, missingSourceTries+1)
	}
}

// A missing commit staged from an earlier job's copy is refused without a fetch from nothing: the
// remote's answer does not depend on what the copy started from.
func TestAJobSourceMissingFromAnEarlierCopyFetchesNoFullHistory(t *testing.T) {
	remote, present := backgroundStagingFixture(t)
	fullFetches := 0
	stager := &privateJobSourceStager{transport: &jobSourceGrantFixture{}, stateRoot: privateStagerRoot(t), remoteForTest: remote,
		fullFetchForTest: func() { fullFetches++ }}
	if err := stager.Stage(context.Background(), "job:earlier", present); err != nil {
		t.Fatal(err)
	}
	missing := missingSource(present, session.SourceCommit)
	executor := &Executor{jobSourceStager: stager}
	for try := 1; try <= missingSourceTries; try++ {
		_, _, err := executor.stageJobSources(context.Background(), "job:missing", []workerproto.JobSource{missing})
		if try == missingSourceTries && !errors.Is(err, ErrRequestRejected) {
			t.Fatalf("try %d = %v, want the job refused", try, err)
		}
	}
	if fullFetches != 0 {
		t.Fatalf("a missing commit fetched the whole history %d times", fullFetches)
	}
}

// A commit missing for a moment, as on a GitHub replica just after a push, stages once it is there.
func TestAJobSourceMissingForAMomentStagesOnceItArrives(t *testing.T) {
	ctx := context.Background()
	remote, source := missingSourceFixture(t, session.SourceCommit)
	// The job's commit exists only in another clone until it is pushed.
	side, sideGit := gitrepo.New(t)
	sideGit("fetch", "-q", remote, "+refs/heads/main:refs/remotes/origin/main")
	sideGit("checkout", "-q", "--detach", "origin/main")
	if err := os.WriteFile(filepath.Join(side, "code"), []byte("arrived"), 0o600); err != nil {
		t.Fatal(err)
	}
	sideGit("add", "code")
	sideGit("commit", "-qm", "arrived")
	arrived, _ := sourceGitValue(ctx, side, "", "file", "rev-parse", "HEAD")
	tree, _ := sourceGitValue(ctx, side, "", "file", "rev-parse", "HEAD^{tree}")
	source.Binding.SelectedCommit, source.Binding.Requested.SHA, source.Binding.AdmittedTree = arrived, arrived, tree

	stager := &privateJobSourceStager{transport: &jobSourceGrantFixture{}, stateRoot: privateStagerRoot(t), remoteForTest: remote}
	for range missingSourceTries - 1 {
		if err := stager.Stage(ctx, "job:missing", source); !errors.Is(err, errSourceGitMissing) {
			t.Fatalf("staging a missing commit = %v", err)
		}
	}
	sideGit("push", "-q", remote, "HEAD:refs/heads/arrived")
	if err := stager.Stage(ctx, "job:missing", source); err != nil {
		t.Fatalf("staging the commit once it arrived = %v", err)
	}
	if len(stager.missing) != 0 {
		t.Fatalf("a staged source kept its missing count: %v", stager.missing)
	}
}

// A remote that cannot be reached is a transfer failure, tried again however often it happens.
func TestAnUnreachableRemoteIsNeverRefusedAsMissing(t *testing.T) {
	remote, source := backgroundStagingFixture(t)
	grants := &jobSourceGrantFixture{}
	stager := &privateJobSourceStager{transport: grants, stateRoot: privateStagerRoot(t), remoteForTest: filepath.Join(t.TempDir(), "absent")}
	executor := &Executor{jobSourceStager: stager}
	for try := 1; try <= missingSourceTries+2; try++ {
		_, _, err := executor.stageJobSources(context.Background(), "job:unreachable", []workerproto.JobSource{source})
		if !errors.Is(err, errArtifactTransfer) || errors.Is(err, ErrRequestRejected) {
			t.Fatalf("try %d = %v, want it tried again", try, err)
		}
	}
	stager.remoteForTest = remote
	if pending, _, err := executor.stageJobSources(context.Background(), "job:unreachable", []workerproto.JobSource{source}); err != nil || len(pending) != 0 {
		t.Fatalf("staging once the remote is back = %v, %d pending", err, len(pending))
	}
}

func TestGitStderrNamesMissingObjectOnlyOnGitsOwnLines(t *testing.T) {
	sha := strings.Repeat("e", 40)
	for stderr, want := range map[string]bool{
		"fatal: remote error: upload-pack: not our ref " + sha + "\n":                               true,
		"error: Server does not allow request for unadvertised object " + sha + "\n":                true,
		"fatal: couldn't find remote ref refs/heads/gone\n":                                         true,
		"remote: fatal: remote error: upload-pack: not our ref " + sha + "\n":                       false,
		"fatal: unable to access 'https://github.com/example/repository.git/': Could not resolve\n": false,
		"fatal: '/absent' does not appear to be a git repository\n":                                 false,
		"": false,
	} {
		tail := &gitStderrTail{}
		// Progress before the fatal line pushes earlier bytes out of the bound.
		_, _ = tail.Write([]byte(strings.Repeat("Receiving objects\r", 1000)))
		_, _ = tail.Write([]byte(stderr))
		if len(tail.tail) > gitStderrTailBytes {
			t.Fatalf("stderr tail kept %d bytes", len(tail.tail))
		}
		if got := tail.namesMissingObject(); got != want {
			t.Errorf("namesMissingObject(%q) = %v, want %v", stderr, got, want)
		}
	}
}

func TestAHostGitFetchOfAMissingCommitSaysSo(t *testing.T) {
	remote, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "one")
	local, _ := gitrepo.New(t)
	err := runSourceGit(context.Background(), local, "", "file", "fetch", "--quiet", "--no-tags", remote, strings.Repeat("e", 40))
	if !errors.Is(err, errSourceGitMissing) {
		t.Fatalf("fetching a missing commit = %v", err)
	}
}
