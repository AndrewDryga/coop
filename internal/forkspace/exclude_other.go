//go:build !darwin && !linux

package forkspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func ExcludeIfRepository(ws, pattern string) error {
	err := Exclude(ws, pattern)
	if errors.Is(err, errNoRepository) {
		return nil
	}
	return err
}

func Exclude(ws, pattern string) error {
	if pattern == "" || strings.ContainsAny(pattern, "\r\n") {
		return errors.New("invalid local Git exclusion")
	}
	excl := filepath.Join(ws, ".git", "info", "exclude")
	if data, err := os.ReadFile(excl); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				return nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return appendFile(excl, []byte("\n# coop: host state, never committed\n"+pattern+"\n"))
}
