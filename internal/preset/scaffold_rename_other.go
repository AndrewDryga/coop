//go:build !darwin && !linux

package preset

import "errors"

func renameScaffoldBundle(_, _ int, _ string) error {
	return errors.New("atomic preset publication is unsupported on this platform")
}
