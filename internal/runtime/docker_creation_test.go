package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestDockerCreationDistinguishesNoSubmissionFromUnknownOutcome(t *testing.T) {
	for _, mode := range []string{"cancelled", "existing", "create-fail", "create-post-fail"} {
		t.Run(mode, func(t *testing.T) {
			fixture := dockerFixture{Mode: mode}
			if mode == "existing" {
				fixture.Container = dockerFixtureContainer()
				fixture.Volume = &DockerVolume{Name: "coop-test", Driver: "local", Scope: "local", CreatedAt: "fixture-time", Labels: dockerFixtureRef().Labels}
			}
			rt, _ := fixtureDocker(t, fixture)
			d, err := BindDocker(context.Background(), rt, fixtureSock(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			ref := dockerFixtureRef()
			ref.ID = ""
			_, err = d.CreateContainer(ctx, DockerCreate{Ref: ref, Image: dockerFixtureContainer().Image})
			wantNotAttempted := mode == "cancelled" || mode == "existing"
			if err == nil || errors.Is(err, ErrDockerCreateNotAttempted) != wantNotAttempted {
				t.Fatal("container creation lost submission boundary", err)
			}
			if mode != "create-post-fail" {
				_, err = d.CreateVolume(ctx, ref)
				if err == nil || errors.Is(err, ErrDockerCreateNotAttempted) != wantNotAttempted {
					t.Fatal("volume creation lost submission boundary", err)
				}
			}
		})
	}
}

// Docker answering a create with a refusal is a known outcome: nothing was created, so the caller
// may take the name's absence as proof. COOP_MEMORY=lots once reached the create, the client refused
// it, and Coop called that "outcome unknown", leaving the run's filesystem helper running for ten
// minutes. A client that fails without saying it refused stays unknown, and Docker's own words
// never reach the error.
func TestDockerCreateTellsARefusalFromAnUnknownOutcome(t *testing.T) {
	for mode, refused := range map[string]bool{"create-refused": true, "create-rejected": true, "create-image-gone": true, "create-fail": false} {
		t.Run(mode, func(t *testing.T) {
			rt, _ := fixtureDocker(t, dockerFixture{Mode: mode})
			d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ref := dockerFixtureRef()
			ref.ID = ""
			id, err := d.CreateContainer(t.Context(), DockerCreate{Ref: ref, Image: dockerFixtureContainer().Image})
			if id != "" || err == nil || errors.Is(err, ErrDockerCreateRefused) != refused || DockerCreateLeftNothing(err) != refused {
				t.Fatalf("create = %q, %v; want refused: %v", id, err, refused)
			}
			// A refusal for an image that is gone says so; any other refusal does not claim it.
			if errors.Is(err, ErrDockerImageMissing) != (mode == "create-image-gone") {
				t.Fatalf("%s: image missing = %v", mode, errors.Is(err, ErrDockerImageMissing))
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "lots") {
				t.Fatalf("Docker's own words reached the error: %v", err)
			}
		})
	}
}

// slowCreateDelay is how long the fixture's "slow-create" daemon takes to answer a create.
const slowCreateDelay = 3 * time.Second

// A create has its own bound. Under load a filtered launch's controller create ran past the ordinary
// 15 seconds while the launch allowed it a minute: the client gave up and Docker finished the create
// anyway, leaving a container cleanup had not waited for. A caller that gives up sooner still decides,
// and that create's outcome stays unknown.
func TestDockerCreateOutlastsTheOrdinaryBound(t *testing.T) {
	ordinary := dockerControlTimeout
	dockerControlTimeout = slowCreateDelay - time.Second
	t.Cleanup(func() { dockerControlTimeout = ordinary })
	bind := func(t *testing.T) (*Docker, DockerRef) {
		t.Helper()
		rt, _ := fixtureDocker(t, dockerFixture{Mode: "slow-create"})
		d, err := BindDocker(context.Background(), rt, fixtureSock(), "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		ref := dockerFixtureRef()
		ref.ID = ""
		return d, ref
	}
	t.Run("past the ordinary bound", func(t *testing.T) {
		d, ref := bind(t)
		if _, err := d.CreateContainer(context.Background(), DockerCreate{Ref: ref, Image: dockerFixtureContainer().Image}); err != nil {
			t.Errorf("container create = %v", err)
		}
		if _, err := d.CreateVolume(context.Background(), ref); err != nil {
			t.Errorf("volume create = %v", err)
		}
	})
	t.Run("past its caller's deadline", func(t *testing.T) {
		d, ref := bind(t)
		ctx, cancel := context.WithTimeout(context.Background(), slowCreateDelay/2)
		defer cancel()
		_, err := d.CreateContainer(ctx, DockerCreate{Ref: ref, Image: dockerFixtureContainer().Image})
		if !errors.Is(err, context.DeadlineExceeded) || DockerCreateLeftNothing(err) {
			t.Errorf("container create = %v, want an unknown outcome", err)
		}
	})
}

// A Ctrl-C that lands while a create's client runs must not kill it. Killed, the client leaves Docker
// finishing the create anyway, and nobody knows whether the container or volume exists: a filtered
// launch stopped that way ended in "outcome is unknown" with its run "cleanup pending" for ten minutes.
// Left to answer, the create tells the caller what Docker made, so cleanup can remove exactly that.
func TestDockerCreateAnswersWhenItsCallerIsCancelledMidRequest(t *testing.T) {
	for _, kind := range []string{"container", "volume"} {
		t.Run(kind, func(t *testing.T) {
			rt, file := fixtureDocker(t, dockerFixture{Mode: "slow-create", CreateDelay: 500 * time.Millisecond})
			d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ref := dockerFixtureRef()
			ref.ID = ""
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type answer struct {
				id  string
				err error
			}
			answered := make(chan answer, 1)
			go func() {
				if kind == "volume" {
					volume, err := d.CreateVolume(ctx, ref)
					answered <- answer{volume.Name, err}
					return
				}
				id, err := d.CreateContainer(ctx, DockerCreate{Ref: ref, Image: dockerFixtureContainer().Image})
				answered <- answer{id, err}
			}()
			wait.ForFile(t, file+".creating") // the client is running, so the request is out
			cancel()
			got := <-answered
			if got.err != nil || got.id == "" {
				t.Fatalf("%s create = %q, %v after a cancel mid-request; want what Docker made, not an unknown outcome", kind, got.id, got.err)
			}
		})
	}
}

// A create is stopped by a cancel only before its request goes out, and by a deadline at any time.
func TestSettleCreateDropsACancelAndKeepsADeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	settled, release, err := settleCreate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cancel()
	if settled.Err() != nil {
		t.Fatalf("a cancel ended a create that was out: %v", settled.Err())
	}
	if _, bounded := settled.Deadline(); bounded {
		t.Fatal("a caller with no deadline gave its create one")
	}

	deadline := time.Now().Add(time.Hour)
	bounded, stop := context.WithDeadline(t.Context(), deadline)
	settled, release, err = settleCreate(bounded)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stop()
	if got, ok := settled.Deadline(); !ok || !got.Equal(deadline) || settled.Err() != nil {
		t.Fatalf("a create under a caller's deadline = %v (bounded %v), error %v; want that deadline and no cancel", got, ok, settled.Err())
	}

	if _, _, err := settleCreate(ctx); !errors.Is(err, ErrDockerCreateNotAttempted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled caller's create = %v, want nothing sent", err)
	}
}

func TestDockerOutputCancelledBeforeSpawnIsNotSubmitted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := dockerOutput(ctx, os.Args[0], nil, 1024, "-test.run=^TestDockerFixtureProcess$")
	if !errors.Is(err, errDockerCommandNotStarted) || !errors.Is(err, context.Canceled) {
		t.Fatal("pre-spawn cancellation reported an unknown daemon outcome", err)
	}
}

func TestDockerStartupRetriesTransientObservation(t *testing.T) {
	rt, file := fixtureDocker(t, dockerFixture{Mode: "transient-start", Container: dockerFixtureContainer()})
	d, err := BindDocker(context.Background(), rt, fixtureSock(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	starts := 0
	code, err := d.StartAttached(context.Background(), dockerFixtureRef(), nil, io.Discard, io.Discard, func() error {
		starts++
		if _, err := os.Stat(file + ".probe-failed"); err != nil {
			return errors.New("fixture did not inject observation failure")
		}
		return os.WriteFile(file+".release", nil, 0600)
	})
	if err != nil || code != 7 || starts != 1 {
		t.Fatal("transient observation killed a healthy workload", code, starts, err)
	}
}

func TestDockerFastExitSupersedesOnlyTransientStartupFailure(t *testing.T) {
	for _, failCallback := range []bool{false, true} {
		rt, file := fixtureDocker(t, dockerFixture{Mode: "transient-fast-exit", Container: dockerFixtureContainer()})
		d, err := BindDocker(context.Background(), rt, fixtureSock(), "")
		if err != nil {
			t.Fatal(err)
		}
		starts := 0
		callbackErr := errors.New("fixture callback failed")
		code, err := d.StartAttached(context.Background(), dockerFixtureRef(), nil, io.Discard, io.Discard, func() error {
			starts++
			if _, err := os.Stat(file + ".probe-failed"); err != nil {
				t.Error("fixture missed transient failure", err)
			}
			if failCallback {
				return callbackErr
			}
			return nil
		})
		_ = d.Close()
		if code != 7 || starts != 1 || (err != nil) != failCallback || (failCallback && !errors.Is(err, callbackErr)) {
			t.Fatal("terminal observation lost workload/callback outcome", failCallback, code, starts, err)
		}
	}
}
