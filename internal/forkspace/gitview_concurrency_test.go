package forkspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGitViewConcurrentProcesses(t *testing.T) {
	if repo := os.Getenv("COOP_TEST_GIT_VIEW_READER"); repo != "" {
		for range 30 {
			cmd, err := GitCommand(t.Context(), repo, "symbolic-ref", "--quiet", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "refs/heads/main" {
				t.Fatalf("branch read: %q, %v", out, err)
			}
		}
		return
	}
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm=%v", warm), func(t *testing.T) {
			repo, _ := viewTestRepo(t)
			t.Setenv(GitViewRootEnv, t.TempDir())
			if warm {
				if _, err := GitCommand(t.Context(), repo, "status"); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			errors := make(chan error, 4)
			for range 4 {
				go func() {
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGitViewConcurrentProcesses$", "-test.count=1")
					cmd.Env = append(os.Environ(), "COOP_TEST_GIT_VIEW_READER="+repo)
					out, err := cmd.CombinedOutput()
					if err != nil {
						err = fmt.Errorf("separate-process read: %w: %s", err, out)
					}
					errors <- err
				}()
			}
			for range 4 {
				if err := <-errors; err != nil {
					t.Error(err)
				}
			}
			entries, err := os.ReadDir(os.Getenv(GitViewRootEnv))
			if err != nil || len(entries) != 1 {
				t.Fatalf("persistent view entries: %v, %v", entries, err)
			}
			for _, name := range []string{"objects", "refs"} {
				link := filepath.Join(os.Getenv(GitViewRootEnv), entries[0].Name(), name)
				if target, err := os.Readlink(link); err != nil || target != filepath.Join(repo, ".git", name) {
					t.Fatalf("view %s target: %q, %v", name, target, err)
				}
			}
		})
	}
}

func TestGitViewTemporaryFilesAreInvocationOwned(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "empty-global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "empty-system"))
	for _, failure := range []string{"", "parse", "cancel"} {
		t.Run("parser-"+failure, func(t *testing.T) {
			dir := t.TempDir()
			canary := filepath.Join(dir, "config.copy")
			if err := os.WriteFile(canary, []byte("other reader owns this\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			data := []byte("[user]\nname = reader\n")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "parse" {
				data = []byte("[invalid\n")
			} else if failure == "cancel" {
				cancel()
			}
			entries, err := listGitConfig(ctx, data, dir)
			if failure == "" && (err != nil || len(entries) != 1 || entries[0] != [2]string{"user.name", "reader"}) {
				t.Fatalf("parser entries=%v, err=%v", entries, err)
			} else if failure != "" && err == nil {
				t.Fatal("failed parser reported success")
			}
			if failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if got, err := os.ReadFile(canary); err != nil || string(got) != "other reader owns this\n" {
				t.Fatalf("parser changed another invocation's scratch: %q, %v", got, err)
			}
			if files, err := os.ReadDir(dir); err != nil || len(files) != 1 {
				t.Fatalf("parser leaked scratch: %v, %v", files, err)
			}
		})
	}
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("publisher-failure=%v", failure), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config")
			canary := path + ".tmp"
			if err := os.WriteFile(canary, []byte("other publisher owns this\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if failure {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			err := writeViewFile(path, []byte("complete published bytes\n"))
			if (err != nil) != failure {
				t.Fatalf("publish failure=%v: %v", failure, err)
			}
			if got, err := os.ReadFile(canary); err != nil || string(got) != "other publisher owns this\n" {
				t.Fatalf("publisher changed another invocation's scratch: %q, %v", got, err)
			}
			if !failure {
				if got, err := os.ReadFile(path); err != nil || string(got) != "complete published bytes\n" {
					t.Fatalf("published bytes: %q, %v", got, err)
				}
				if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("published privacy: %v, %v", info, err)
				}
			}
			if files, err := os.ReadDir(dir); err != nil || len(files) != 2 {
				t.Fatalf("publisher leaked scratch: %v, %v", files, err)
			}
		})
	}
}

func TestGitViewConcurrentLinksRejectForeignEntries(t *testing.T) {
	dir := t.TempDir()
	source, link := filepath.Join(dir, "source"), filepath.Join(dir, "link")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	start, results := make(chan struct{}), make(chan error, 64)
	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() {
			<-start
			results <- ensureSymlink(link, source, true)
		})
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}
	if target, err := os.Readlink(link); err != nil || target != source {
		t.Fatalf("concurrent link target: %q, %v", target, err)
	}
	for _, regular := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign-regular=%v", regular), func(t *testing.T) {
			foreign := filepath.Join(t.TempDir(), "entry")
			var err error
			if regular {
				err = os.WriteFile(foreign, []byte("held"), 0o600)
			} else {
				err = os.Symlink(dir, foreign)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := ensureSymlink(foreign, source, true); err == nil {
				t.Fatal("foreign entry was accepted")
			}
		})
	}
}

func TestGitViewRefreshPreservesOperationHead(t *testing.T) {
	repo, _ := viewTestRepo(t)
	if _, err := GitCommand(t.Context(), repo, "status"); err != nil {
		t.Fatal(err)
	}
	view := gitViews[filepath.Join(repo, ".git")]
	realHead := readOut(t, repo, "symbolic-ref", "HEAD")
	operation := filepath.Join(view.dir, "rebase-merge")
	if err := os.Mkdir(operation, 0o700); err != nil {
		t.Fatal(err)
	}
	detached := readOut(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(view.dir, "HEAD"), detached, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GitCommand(t.Context(), repo, "status"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(view.dir, "HEAD")); err != nil || string(got) != string(detached) {
		t.Fatalf("refresh changed operation HEAD: %q, %v", got, err)
	}
	if got := readOut(t, repo, "symbolic-ref", "HEAD"); string(got) != string(realHead) {
		t.Fatalf("refresh changed real HEAD: %q", got)
	}
	if info, err := os.Stat(operation); err != nil || !info.IsDir() {
		t.Fatalf("refresh removed operation state: %v, %v", info, err)
	}
}

func TestGitViewConcurrentOptionalLinkRemoval(t *testing.T) {
	dir := t.TempDir()
	missing, link := filepath.Join(dir, "missing"), filepath.Join(dir, "link")
	for range 20 {
		if err := os.Symlink(missing, link); err != nil {
			t.Fatal(err)
		}
		start, results := make(chan struct{}), make(chan error, 32)
		var workers sync.WaitGroup
		for range 32 {
			workers.Go(func() {
				<-start
				results <- ensureSymlink(link, missing, false)
			})
		}
		close(start)
		workers.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Error(err)
			}
		}
		if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("optional link was not removed: %v", err)
		}
	}
}
