//go:build !darwin && !linux

package networkstate

import (
	"errors"
	"os"
)

func renameExclusive(*os.File, string, string) error {
	return errors.New("immutable network launch publication is unsupported on this host")
}
