//go:build !darwin && !linux

package forkspace

import "errors"

func renameNoReplace(_, _ string) error {
	return errors.New("atomic no-replace rename is unsupported on this platform")
}
