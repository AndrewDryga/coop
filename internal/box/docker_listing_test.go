package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestAuthorityPathMissingTailDoesNotHideDanglingAlias(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "removed"), alias); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{alias, filepath.Join(alias, "nested")} {
		if _, err := resolveAuthorityPath(source); err == nil {
			t.Fatal("dangling alias reconstructed as an unrelated missing tail")
		}
	}
}

// quickDockerListing shortens the patient listing's limits for a test that expects to exhaust them.
func quickDockerListing(t *testing.T) {
	t.Helper()
	attempt, limit, pause := dockerListingAttemptLimit, dockerListingLimit, dockerListingPause
	t.Cleanup(func() { dockerListingAttemptLimit, dockerListingLimit, dockerListingPause = attempt, limit, pause })
	dockerListingAttemptLimit, dockerListingLimit, dockerListingPause = time.Second, 30*time.Second, 10*time.Millisecond
}

func TestDockerListingKeepsItsOverallLimit(t *testing.T) {
	quickDockerListing(t)
	dockerListingAttemptLimit, dockerListingLimit = time.Minute, 200*time.Millisecond
	calls := 0
	err := listDockerPatiently(t.Context(), func(ctx context.Context) error {
		calls++
		<-ctx.Done()
		return ctx.Err()
	})
	var refusal *ui.UsageError
	if !errors.As(err, &refusal) || refusal.Headline != "Docker did not answer in time" {
		t.Fatalf("err = %v", err)
	}
	if calls > 1 {
		t.Fatalf("passes = %d after the overall limit, want no second one", calls)
	}
}

func TestDockerListingStopsWhenTheCallerCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	err := listDockerPatiently(ctx, func(ctx context.Context) error {
		calls++
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err = %v after %d calls", err, calls)
	}
}

// The account's own runtime check runs first on the same launch, so it needs the same patience.
func TestCredentialContainerCheckToleratesASlowDocker(t *testing.T) {
	const failOnce = `[ -e "$0.churned" ] || { : > "$0.churned"; exit 1; }`
	for _, test := range []struct {
		name, ps, headline string
		busy               bool
	}{
		{name: "asks again after a failed pass", ps: failOnce},
		{name: "a container that holds it is still found", ps: failOnce + "; printf '%s\\n' abc", busy: true},
		{name: "never answers", ps: "exec sleep 300", headline: "Docker did not answer in time"},
		{name: "fails", ps: "exit 1", headline: "Docker could not list its containers"},
	} {
		t.Run(test.name, func(t *testing.T) {
			quickDockerListing(t)
			path := filepath.Join(t.TempDir(), "docker")
			script := "#!/bin/sh\ncase \"$*\" in\n*\"ps -q\"*) " + test.ps + " ;;\n*inspect*) printf '%s\\n' '{\"k\":\"v\"}' ;;\nesac\n"
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			err := checkCredentialContainers(t.Context(), runtime.Runtime{Name: path}, "claude", "key", true)
			var refusal *ui.UsageError
			switch {
			case test.headline != "":
				if !errors.As(err, &refusal) || refusal.Headline != test.headline {
					t.Fatalf("err = %v, want %q", err, test.headline)
				}
			case test.busy:
				if err == nil || !strings.Contains(err.Error(), "credential is busy") {
					t.Fatalf("err = %v, want busy", err)
				}
			case err != nil:
				t.Fatalf("err = %v", err)
			}
		})
	}
}
