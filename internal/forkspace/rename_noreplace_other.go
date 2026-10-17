//go:build !darwin && !linux

package forkspace

import "errors"

func renameAtNoReplace(int, string, int, string) error {
	return errors.New("atomic no-replace rename is unsupported on this platform")
}
