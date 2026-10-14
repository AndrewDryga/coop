package procharness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func binarySource(t *testing.T, marker string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":      "module fixture\n\ngo 1.27\n",
		"main.go":     "package main\nimport \"fmt\"\nfunc main(){fmt.Println(marker)}\n",
		"ordinary.go": "//go:build !cooplivetest\n\npackage main\nconst marker = " + strconv.Quote(marker) + "\n",
		"live.go":     "//go:build cooplivetest\n\npackage main\nconst marker = \"tagged\"\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func instrumentGo(t *testing.T, before string) string {
	t.Helper()
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "builds")
	if os.Getenv("CACHE_BUILD_MARKER") == "" {
		t.Setenv("CACHE_BUILD_MARKER", "unspecified")
	}
	// The wrapper observes actual builds, not a production injection hook.
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*|$CACHE_BUILD_MARKER\" >> " + shellQuote(log) + "\n" +
		before + "\nexec " + shellQuote(realGo) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestBinaryCacheReusesBytesWithoutSharingCaseFiles(t *testing.T) {
	t.Setenv("CACHE_BUILD_MARKER", "captured")
	log := instrumentGo(t, "")
	cache, err := NewBinaryCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CACHE_BUILD_MARKER", "changed")
	root := binarySource(t, "ordinary")
	var first os.FileInfo
	for i, tags := range [][]string{nil, nil, {"cooplivetest"}, {"cooplivetest", "cooplivetest"}} {
		dest := filepath.Join(t.TempDir(), "fixture")
		if err := cache.Copy(t.Context(), root, dest, ".", tags...); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(dest)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o700 {
			t.Fatalf("copy is not an independent private executable: %v, %v", info, err)
		}
		if first != nil && os.SameFile(first, info) {
			t.Fatal("cache/cases share a writable inode")
		}
		want := "ordinary\n"
		if len(tags) != 0 {
			want = "tagged\n"
		}
		if out, err := exec.Command(dest).CombinedOutput(); err != nil || string(out) != want {
			t.Fatalf("wrong binary identity: %q, %v, want %q", out, err, want)
		}
		if i == 0 {
			first = info
			if err := os.WriteFile(dest, []byte("case-local damage"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := cache.Copy(t.Context(), root, dest, ".", tags...); !os.IsExist(err) {
			t.Fatalf("existing case file was overwritten: %v", err)
		}
	}
	other := binarySource(t, "other-root")
	dest := filepath.Join(t.TempDir(), "fixture")
	if err := cache.Copy(t.Context(), other, dest, "."); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(dest).Output(); err != nil || string(out) != "other-root\n" {
		t.Fatalf("source roots aliased: %q, %v", out, err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || strings.Count(string(data), "|captured") != 3 || strings.Contains(string(data), "|changed") {
		t.Fatalf("expected exactly ordinary/tagged/other-root builds with frozen environment: %s", data)
	}
}

func TestBinaryCacheFailureCannotPublishOrPoisonAResult(t *testing.T) {
	cache, err := NewBinaryCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	root := binarySource(t, "repaired")
	source := filepath.Join(root, "main.go")
	good, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package main\nthis is broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "fixture")
	if err := cache.Copy(t.Context(), root, dest, "."); err == nil {
		t.Fatal("failed compilation was accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) || len(cache.files) != 0 {
		t.Fatalf("failed compilation published a copy/cache entry: %v, %v", err, cache.files)
	}
	if files, err := os.ReadDir(cache.dir); err != nil || len(files) != 0 {
		t.Fatalf("failed compilation left partial output: %v, %v", files, err)
	}
	if err := os.WriteFile(source, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cache.Copy(t.Context(), root, dest, "."); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(dest).Output(); err != nil || string(out) != "repaired\n" {
		t.Fatalf("failed build poisoned later success: %q, %v", out, err)
	}
}

func TestBinaryCacheAbortReapsCompilerAndLeavesNoSuccess(t *testing.T) {
	state := t.TempDir()
	ready := filepath.Join(state, "ready")
	release := filepath.Join(state, "release")
	instrumentGo(t, "if [ ! -f "+shellQuote(release)+" ]; then printf '%s\\n' $$ > "+shellQuote(ready)+"; while :; do sleep 30; done; fi")
	cache, err := NewBinaryCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	root := binarySource(t, "after-abort")
	dest := filepath.Join(t.TempDir(), "fixture")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cache.Copy(ctx, root, dest, ".") }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Error("compiler cleanup did not finish")
		}
	})
	pid, err := strconv.Atoi(strings.TrimSpace(awaitFileContent(t, ready)))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		done <- err // retain the completion receipt if an assertion aborts the test
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("abort was not reported: %v", err)
		}
	case <-time.After(wait.Deadline):
		t.Fatal("aborted compiler did not drain")
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("owned compiler process group survived: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) || len(cache.files) != 0 {
		t.Fatalf("aborted compilation published success: %v, %v", err, cache.files)
	}
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cache.Copy(t.Context(), root, dest, "."); err != nil {
		t.Fatalf("abort poisoned retry: %v", err)
	}
}
