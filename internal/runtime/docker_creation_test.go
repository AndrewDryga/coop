package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
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
			d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
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
	d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
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
		d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
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
