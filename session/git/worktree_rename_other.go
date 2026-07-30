//go:build !linux && !darwin

package git

import "errors"

func renameAtNoReplace(_ int, _ string, _ int, _ string) error {
	return errors.New("atomic no-replace directory rename is unsupported on this platform")
}
