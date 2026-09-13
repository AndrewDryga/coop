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
// stream. The returned bytes exclude only the terminal line ending.
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
	done := make(chan struct{})
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		select {
		case sig := <-interrupts:
			_ = restoreEcho()
			fmt.Fprintln(os.Stderr)
			signal.Stop(interrupts)
			if process, err := os.FindProcess(os.Getpid()); err == nil {
				_ = process.Signal(sig)
			}
		case <-done:
		}
	}()
	reader := bufio.NewReader(io.LimitReader(os.Stdin, secretInputLimit+1))
	line, readErr := reader.ReadString('\n')
	signal.Stop(interrupts)
	close(done)
	restored := restoreEcho()
	fmt.Fprintln(os.Stderr)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, fmt.Errorf("read secret input: %w", readErr)
	}
	if restored != nil {
		return nil, fmt.Errorf("restore terminal echo: %w", restored)
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return []byte(line), nil
}
