package acpproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyNaturalExitRetiresFactoryContextBeforeReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input, editor := io.Pipe()
	defer input.Close()
	defer editor.Close()
	first := newFakeChild()
	defer first.child().Stop()
	started := make(chan context.Context, 1)
	replacing := make(chan error, 1)
	var lifetime context.Context
	factory := func(attempt context.Context) (*Child, error) {
		if lifetime == nil {
			lifetime = attempt
			started <- attempt
			return first.child(), nil
		}
		replacing <- lifetime.Err()
		<-attempt.Done()
		return nil, attempt.Err()
	}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, input, io.Discard, factory, nil) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first factory did not start")
	}
	_ = first.outW.Close() // natural EOF, not a selector-triggered Stop
	select {
	case err := <-replacing:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("retired child's factory context survived into replacement wait: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("natural exit did not begin replacement")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("replacement wait did not stop")
	}
}

func TestProxyReplacementWaitCanBeInterrupted(t *testing.T) {
	for _, action := range []string{"selection", "reload", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			input, editor := io.Pipe()
			output, proxyOutput := io.Pipe()
			defer input.Close()
			defer editor.Close()
			defer output.Close()
			defer proxyOutput.Close()
			first, replacement := newFakeChild(), newFakeChild()
			defer first.child().Stop()
			defer replacement.child().Stop()
			started := make(chan struct{})
			newest := make(chan context.Context, 1)
			var attempts atomic.Int32
			factory := func(attempt context.Context) (*Child, error) {
				switch attempts.Add(1) {
				case 1:
					return first.child(), nil
				case 2:
					close(started)
					<-attempt.Done()
					return nil, attempt.Err()
				default:
					newest <- attempt
					return replacement.child(), nil
				}
			}
			hooks := &Hooks{FromEditor: func(line []byte) (bool, []byte, []byte, bool) {
				if bytes.Contains(line, []byte("__switch__")) {
					return true, successResponse(string(parse(line).ID)), nil, true
				}
				return false, nil, nil, false
			}}
			reload := make(chan struct{})
			done := make(chan error, 1)
			go func() { done <- RunWith(ctx, input, proxyOutput, factory, hooks, RunOpts{Reload: reload}) }()
			editorOutput := bufio.NewReader(output)
			writeLine(t, editor, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
			readLine(t, bufio.NewReader(first.inR))
			writeLine(t, first.outW, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			readLine(t, editorOutput)
			writeLine(t, editor, `{"jsonrpc":"2.0","id":2,"method":"session/set_config_option","params":{"configId":"__switch__"}}`)
			readLine(t, editorOutput)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("replacement factory did not enter its wait")
			}
			switch action {
			case "selection":
				writeLine(t, editor, `{"jsonrpc":"2.0","id":3,"method":"session/set_config_option","params":{"configId":"__switch__"}}`)
				readLine(t, editorOutput)
				select {
				case lifetime := <-newest:
					if lifetime.Err() != nil {
						t.Fatal("successful factory context was cancelled before its child retired")
					}
				case <-time.After(time.Second):
					t.Fatal("provider selection could not interrupt the obsolete quota wait")
				}
				init := parse(readLine(t, bufio.NewReader(replacement.inR)))
				writeLine(t, replacement.outW, `{"jsonrpc":"2.0","id":`+string(init.ID)+`,"result":{}}`)
				_ = editor.Close()
			case "reload":
				close(reload)
			case "disconnect":
				_ = editor.Close()
			}
			select {
			case err := <-done:
				if action == "reload" {
					if _, ok := ReloadSnapshot(err); !ok {
						t.Fatalf("reload during wait = %v, want resumable reload", err)
					}
				} else if err != nil {
					t.Fatalf("shutdown after %s = %v", action, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("%s could not interrupt the replacement wait", action)
			}
		})
	}
}
