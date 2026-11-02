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
