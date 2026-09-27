package workerconnector

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func sourceRefreshFixture(t *testing.T) (*privateJobSourceStager, workerproto.JobSource, string, func(...string)) {
	t.Helper()
	remote, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "original")
	source := testJobSource()
	value := func(ref string) string {
		t.Helper()
		value, err := sourceGitValue(context.Background(), remote, "", "file", "rev-parse", ref)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	head := value("HEAD")
	source.Binding.DefaultCommit, source.Binding.SelectedCommit, source.Binding.BaseCommit = head, head, head
	source.Binding.AdmittedTree = value("HEAD^{tree}")
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	stager := &privateJobSourceStager{transport: &jobSourceGrantFixture{}, stateRoot: root, remoteForTest: remote}
	if err := stager.Stage(context.Background(), "job:one", source); err != nil {
		t.Fatal(err)
	}
	key, _ := source.StagingKey()
	return stager, source, filepath.Join(root, "job-sources", key, "repository"), git
}

func TestRepositoryRefreshUsesAnExistingVerifiedCommitWithoutFetching(t *testing.T) {
	s, source, repository, _ := sourceRefreshFixture(t)
	s.gitForTest = func(ctx context.Context, dir, token, protocol string, args ...string) (string, error) {
		if args[0] == "fetch" {
			t.Fatal("fetched an existing verified default commit")
		}
		return sourceGitValue(ctx, dir, token, protocol, args...)
	}
	head, err := s.RefreshDefault(context.Background(), "job:one", source, repository)
	if err != nil || head != source.Binding.DefaultCommit {
		t.Fatalf("refresh = %s, %v", head, err)
	}
	if s.transport.(*jobSourceGrantFixture).calls != 2 {
		t.Fatal("refresh reused the create credential without current job authorization")
	}
}

func TestRepositoryFetchMayOutliveRemoteIdentityLookup(t *testing.T) {
	s, source, repository, git := sourceRefreshFixture(t)
	git("commit", "--allow-empty", "-qm", "advanced")
	want, err := sourceGitValue(context.Background(), s.remoteForTest, "", "file", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(filepath.Dir(repository), "source.json")
	before, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var lookup context.Context
	fetched := false
	s.gitForTest = func(ctx context.Context, dir, token, protocol string, args ...string) (string, error) {
		if args[0] == "ls-remote" {
			lookup = ctx
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("identity lookup has no deadline")
			}
		}
		if args[0] == "fetch" {
			fetched = true
			if lookup == nil || lookup.Err() == nil || ctx.Err() != nil {
				t.Fatal("object transfer reused the completed lookup context")
			}
			if _, ok := ctx.Deadline(); ok || !slices.Contains(args, "--no-write-fetch-head") || args[len(args)-1] != want {
				t.Fatal("transfer must fetch the exact commit with independent stall bounds and no ref mutation")
			}
		}
		return sourceGitValue(ctx, dir, token, protocol, args...)
	}
	head, err := s.RefreshDefault(context.Background(), "job:one", source, repository)
	if err != nil || head != want || !fetched {
		t.Fatalf("current default = %s, want %s; fetched=%v, err=%v", head, want, fetched, err)
	}
	after, err := os.ReadFile(receiptPath)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("refresh rewrote the frozen source receipt")
	}
	if err := verifySourceRepository(context.Background(), repository, source); err != nil {
		t.Fatal("refresh changed the selected source:", err)
	}
}

func TestRepositoryRefreshRefusesUnprovenOrUnavailableAuthority(t *testing.T) {
	for _, scenario := range []string{"wrong-path", "expired", "cancelled", "wrong-ref", "multiple-refs", "unavailable", "lookup-deadline"} {
		t.Run(scenario, func(t *testing.T) {
			s, source, repository, _ := sourceRefreshFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := repository
			switch scenario {
			case "wrong-path":
				path = t.TempDir()
			case "expired":
				s.transport.(*jobSourceGrantFixture).expired = true
			case "cancelled":
				cancel()
			case "unavailable":
				s.remoteForTest = filepath.Join(t.TempDir(), "missing")
			default:
				s.lookupTimeoutForTest = time.Millisecond
				s.gitForTest = func(ctx context.Context, _ string, _ string, _ string, args ...string) (string, error) {
					if args[0] != "ls-remote" {
						t.Fatal("unproven lookup reached an object operation")
					}
					if scenario == "lookup-deadline" {
						<-ctx.Done()
						return "", ctx.Err()
					}
					ref := "refs/heads/other"
					if scenario == "multiple-refs" {
						ref = source.Binding.DefaultRef + "\n" + source.Binding.DefaultCommit + "\t" + source.Binding.DefaultRef
					}
					return source.Binding.DefaultCommit + "\t" + ref, nil
				}
			}
			if _, err := s.RefreshDefault(ctx, "job:one", source, path); err == nil {
				t.Fatal("accepted unproven refresh authority")
			} else if scenario == "lookup-deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lookup timeout = %v", err)
			}
			if err := verifySourceRepository(context.Background(), repository, source); err != nil {
				t.Fatal("failed refresh changed frozen source:", err)
			}
		})
	}
}

func TestRepositoryFetchRemainsCancellableAtItsOwnDeadline(t *testing.T) {
	s, source, repository, git := sourceRefreshFixture(t)
	git("commit", "--allow-empty", "-qm", "advanced")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.gitForTest = func(ctx context.Context, dir, token, protocol string, args ...string) (string, error) {
		if args[0] == "fetch" {
			cancel()
		}
		return sourceGitValue(ctx, dir, token, protocol, args...)
	}
	if _, err := s.RefreshDefault(ctx, "job:one", source, repository); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fetch = %v", err)
	}
	cmd := sourceGitCommand(context.Background(), repository, "https", "fetch")
	for _, setting := range []string{
		"GIT_CONFIG_KEY_1=http.lowSpeedLimit", "GIT_CONFIG_VALUE_1=1",
		"GIT_CONFIG_KEY_2=http.lowSpeedTime", "GIT_CONFIG_VALUE_2=120",
	} {
		if !slices.Contains(cmd.Env, setting) {
			t.Fatal("transfer lacks its HTTP stall bound:", setting)
		}
	}
}

func TestSourceGitCancellationKillsCredentialedHelpers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := sourceGitCommand(ctx, "", "https")
	cmd.Path, cmd.Args = "/bin/sh", []string{"sh", "-c", "sleep 60 & printf '%s\\n' \"$!\"; wait"}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid helper pid: %q, %v", line, err)
	}
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("cancelled Git command succeeded")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("credentialed Git helper survived cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
