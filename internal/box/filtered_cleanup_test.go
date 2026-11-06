package box

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestFilteredCleanupJoinsFilesystemBeforeSeal(t *testing.T) {
	for _, fault := range []string{"none", "error", "panic"} {
		t.Run(fault, func(t *testing.T) {
			f, d := filteredFixture(t)
			if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			id := f.record.ID
			agentName := f.ref("agent").Name
			entered, release, progress := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			d.beforeCall = func(op string) {
				if op == "remove:observations" {
					once.Do(func() { close(progress) })
				}
			}
			sentinel := errors.New("filesystem retirement failed")
			type outcome struct {
				gone       bool
				err        error
				panicValue any
			}
			done := make(chan outcome, 1)
			go func() {
				var result outcome
				defer func() { result.panicValue = recover(); done <- result }()
				result.gone, result.err = f.cleanupWithFilesystem("exited", func() error {
					d.mu.Lock()
					_, present := d.containers[agentName]
					d.mu.Unlock()
					if present {
						t.Error("filesystem retired before shared consumer removal")
					}
					close(entered)
					<-release
					if fault == "panic" {
						panic("retirement panic")
					}
					if fault == "error" {
						return sentinel
					}
					return nil
				})
			}()
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			for _, ch := range []chan struct{}{entered, progress} {
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("cleanup branches did not overlap")
				}
			}
			record, err := f.store.Execution(id)
			if err != nil || record.Receipt != nil {
				t.Fatal("receipt sealed before filesystem settled", err)
			}
			select {
			case <-done:
				t.Fatal("cleanup returned before retirement")
			default:
			}
			close(release)
			select {
			case result := <-done:
				if fault == "panic" {
					if result.panicValue == nil {
						t.Fatal("lost retirement panic")
					}
					return
				}
				if !result.gone || result.panicValue != nil || (fault == "error" && !errors.Is(result.err, sentinel)) || (fault == "none" && result.err != nil) {
					t.Fatalf("outcome %+v", result)
				}
				record, err = f.store.Execution(id)
				if err != nil || record.Receipt == nil {
					t.Fatal("receipt not sealed after join", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cleanup did not finish")
			}
		})
	}
}

func TestFilteredCleanupRetiresFilesystemAfterEarlyFailure(t *testing.T) {
	f, _ := filteredFixture(t)
	if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.store.Path(), "execution-"+f.record.ID+".json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	sentinel := errors.New("filesystem failed")
	_, err := f.cleanupWithFilesystem("exited", func() error { calls++; return sentinel })
	if calls != 1 || !errors.Is(err, sentinel) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestFilteredCleanupRetainsUncertainConsumer(t *testing.T) {
	f, d := filteredFixture(t)
	if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	d.refuseRemoval = "agent"
	calls := 0
	gone, err := f.cleanupWithFilesystem("exited", func() error {
		calls++
		d.mu.Lock()
		defer d.mu.Unlock()
		stopped := false
		for _, op := range d.log {
			if op == "stop:controller" {
				stopped = true
			}
		}
		if !stopped {
			t.Error("filesystem retirement raced uncertain network containment")
		}
		return errors.New("filesystem consumer still present")
	})
	if gone || err == nil || calls != 1 {
		t.Fatalf("gone=%v calls=%d err=%v", gone, calls, err)
	}
	record, err := f.store.Execution(f.record.ID)
	if err != nil || record.Receipt == nil || record.Receipt.Cleanup != "pending" {
		t.Fatal("uncertain cleanup claimed complete", err)
	}
}

// On a host with about 58 containers, a run whose command succeeded ended with "✗ inspect Docker
// filesystem consumers: context deadline exceeded", the deadline again on its own line, and exit 1:
// its teardown ran out of time checking the host's other containers. The protection and its record
// stay for the next run's sweep, so the teardown says so in one line and the run keeps its status.
// Any other retirement failure is still the run's.
func TestATeardownInventoryThatRunsOutOfTimeKeepsTheRunsStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retire  error
		failed  bool
		warning string
	}{
		{name: "out of time", retire: runtime.ErrConsumerInventoryUnfinished,
			warning: "⚠ Coop could not check every Docker container in time, so this box's filesystem protection stays until the next run in this project removes it\n"},
		{name: "another failure", retire: errors.New("filesystem consumer still present"), failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := filteredFixture(t)
			if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			var gone bool
			var err error
			out := captureStderr(t, func() {
				gone, err = f.cleanupWithFilesystem("exited", func() error { return tc.retire })
			})
			if !gone || (err != nil) != tc.failed || tc.failed && !errors.Is(err, tc.retire) {
				t.Fatalf("cleanup = (%v, %v)", gone, err)
			}
			if out != tc.warning {
				t.Fatalf("teardown printed %q, want %q", out, tc.warning)
			}
		})
	}
}
