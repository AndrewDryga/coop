package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
)

const secretInputLimit = 64 << 10

// ReadSecret reads one terminal line with echo disabled. It never accepts a pipe or redirected
// input: credentials must not be smuggled through process arguments, shell history or a shared
// stream. The returned bytes exclude only the terminal line ending. Ctrl-C, or Ctrl-D before
// anything was typed, is ErrCancelled: the person gave no secret, and Coop used to die of the first
// and take the second for an empty one.
func ReadSecret(prompt string) ([]byte, error) {
	if !IsTerminal(os.Stdin) {
		return nil, errors.New("secret input requires an interactive terminal")
	}
	fmt.Fprint(os.Stderr, prompt)
	restore, err := disableTerminalEcho(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr)
		return nil, fmt.Errorf("hide secret input: %w", err)
	}
	var (
		restoreOnce sync.Once
		restoreErr  error
	)
	restoreEcho := func() error {
		restoreOnce.Do(func() { restoreErr = restore() })
		return restoreErr
	}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	type input struct {
		line string
		err  error
	}
	typed := make(chan input, 1)
	go func() {
		reader := bufio.NewReader(io.LimitReader(os.Stdin, secretInputLimit+1))
		line, err := reader.ReadString('\n')
		typed <- input{line, err}
	}()
	var got input
	interrupted := false
	select {
	case got = <-typed:
	case <-interrupts:
		interrupted = true
	}
	signal.Stop(interrupts) // a second Ctrl-C takes the default action
	if interrupted {
		// The read stays blocked on the terminal until the process ends, and nothing reads the
		// terminal after a cancelled prompt.
		_ = restoreEcho()
		fmt.Fprintln(os.Stderr)
		return nil, ErrCancelled
	}
	restored := restoreEcho()
	fmt.Fprintln(os.Stderr)
	if got.err != nil && !errors.Is(got.err, io.EOF) {
		return nil, fmt.Errorf("read secret input: %w", got.err)
	}
	if restored != nil {
		return nil, fmt.Errorf("restore terminal echo: %w", restored)
	}
	if got.line == "" && errors.Is(got.err, io.EOF) {
		return nil, ErrCancelled
	}
	line := strings.TrimSuffix(got.line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return []byte(line), nil
}
