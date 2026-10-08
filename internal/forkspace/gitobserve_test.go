package forkspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitObservationAndPinnedTransportNeverReconcileSource(t *testing.T) {
	for _, operation := range []string{"observe", "clone", "fetch"} {
		t.Run(operation, func(t *testing.T) {
			repo, _ := viewTestRepo(t)
			original := readOut(t, repo, "rev-parse", "HEAD")
			if _, err := viewRun(t, repo, "status", "--porcelain"); err != nil {
				t.Fatal(err)
			}
			view := gitViews[filepath.Join(repo, ".git")]
			// A newer cached HEAD would be copied back by the ordinary view.
			cached := filepath.Join(view.dir, "HEAD")
			if err := os.WriteFile(cached, original, 0o600); err != nil {
				t.Fatal(err)
			}
			future := time.Now().Add(time.Hour)
			if err := os.Chtimes(cached, future, future); err != nil {
				t.Fatal(err)
			}
			headPath, indexPath := filepath.Join(repo, ".git", "HEAD"), filepath.Join(repo, ".git", "index")
			head, _ := os.ReadFile(headPath)
			index, _ := os.ReadFile(indexPath)
			commit := strings.TrimSpace(string(original))
			switch operation {
			case "observe":
				out, err := ObserveGit(context.Background(), repo, "status", "--porcelain")
				if err != nil || len(out) != 0 {
					t.Fatalf("observation: %q, %v", out, err)
				}
			case "clone":
				if err := GitClonePinnedContext(context.Background(), repo, filepath.Join(t.TempDir(), "copy"), commit); err != nil {
					t.Fatal(err)
				}
			case "fetch":
				dst, _ := viewTestRepo(t)
				if err := GitFetchPinnedContext(context.Background(), repo, dst, commit); err != nil {
					t.Fatal(err)
				}
			}
			gotHead, _ := os.ReadFile(headPath)
			gotIndex, _ := os.ReadFile(indexPath)
			if !bytes.Equal(head, gotHead) || !bytes.Equal(index, gotIndex) {
				t.Fatal("observation/transport changed source HEAD or index")
			}
		})
	}
}

func TestObservedCheckoutConvertsNativeEOLWithoutExecutingDrivers(t *testing.T) {
	repo, run := viewTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("tracked.txt text eol=crlf filter=audit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".gitattributes")
	run("commit", "-qm", "trusted conversion")
	object := strings.TrimSpace(string(readOut(t, repo, "rev-parse", "HEAD:tracked.txt")))
	marker := filepath.Join(t.TempDir(), "executed")
	script := filepath.Join(repo, "driver.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf invoked > '"+marker+"'\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	run("config", "filter.audit.smudge", script)
	run("config", "filter.audit.required", "true")
	before := readOut(t, repo, "ls-files", "--stage")
	var converted bytes.Buffer
	if err := ObserveCheckoutBlob(t.Context(), repo, object, "tracked.txt", &converted); err != nil || converted.String() != "before\r\n" {
		t.Fatalf("native conversion: %q %v", converted.String(), err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("observation executed an external driver: %v", err)
	}
	if after := readOut(t, repo, "ls-files", "--stage"); !bytes.Equal(before, after) {
		t.Fatal("checkout observation mutated index")
	}
	// Exact same command against the native config proves the planted driver works.
	run("cat-file", "--filters", "--path=tracked.txt", object)
	if data, err := os.ReadFile(marker); err != nil || string(data) != "invoked" {
		t.Fatalf("driver positive control failed: %q %v", data, err)
	}
}

func TestObservedCheckoutProjectsOnlySafeTrustedIncludedSettings(t *testing.T) {
	repo, run := viewTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("tracked.txt text filter=audit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".gitattributes")
	run("commit", "-qm", "native text")
	object := strings.TrimSpace(string(readOut(t, repo, "rev-parse", "HEAD:tracked.txt")))
	marker := filepath.Join(t.TempDir(), "executed")
	script := filepath.Join(repo, "driver.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf invoked > '"+marker+"'\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	included := filepath.Join(t.TempDir(), "trusted-include")
	run("config", "--file", included, "core.autocrlf", "true")
	run("config", "--file", included, "filter.audit.smudge", script)
	run("config", "--file", included, "filter.audit.required", "true")
	run("config", "--file", os.Getenv("GIT_CONFIG_GLOBAL"), "include.path", included)
	before := readOut(t, repo, "ls-files", "--stage")
	var converted bytes.Buffer
	if err := ObserveCheckoutBlob(t.Context(), repo, object, "tracked.txt", &converted); err != nil || converted.String() != "before\r\n" {
		t.Fatalf("safe included data conversion: %q %v", converted.String(), err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("included trusted executable config entered sterile observation: %v", err)
	}
	if after := readOut(t, repo, "ls-files", "--stage"); !bytes.Equal(before, after) {
		t.Fatal("trusted-setting projection mutated parent index")
	}
	run("-c", "include.path="+included, "cat-file", "--filters", "--path=tracked.txt", object)
	if data, err := os.ReadFile(marker); err != nil || string(data) != "invoked" {
		t.Fatalf("trusted included driver positive control: %q %v", data, err)
	}
}

func TestObservedCheckoutHandlesTrustedSettingFailuresAndPrecedence(t *testing.T) {
	for _, kind := range []string{"system", "global-override", "no-system", "invalid-value", "malformed", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			repo, run := viewTestRepo(t)
			run("config", "--file", os.Getenv("GIT_CONFIG_SYSTEM"), "core.autocrlf", "true")
			object := strings.TrimSpace(string(readOut(t, repo, "rev-parse", "HEAD:tracked.txt")))
			ctx := t.Context()
			want, failure := "before\r\n", false
			switch kind {
			case "global-override":
				run("config", "--file", os.Getenv("GIT_CONFIG_GLOBAL"), "core.autocrlf", "false")
				want = "before\n"
			case "no-system":
				t.Setenv("GIT_CONFIG_NOSYSTEM", "true")
				want = "before\n"
			case "invalid-value":
				run("config", "--file", os.Getenv("GIT_CONFIG_GLOBAL"), "core.eol", "invalid")
				failure = true
			case "malformed":
				if err := os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"), []byte("[malformed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				failure = true
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx, failure = cancelled, true
			}
			var converted bytes.Buffer
			err := ObserveCheckoutBlob(ctx, repo, object, "tracked.txt", &converted)
			if failure {
				if err == nil {
					t.Fatal("uncertain trusted config silently ignored")
				}
				if kind == "cancelled" && !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost", err)
				}
			} else if err != nil || converted.String() != want {
				t.Fatalf("safe native precedence: %q want %q %v", converted.String(), want, err)
			}
		})
	}
}
