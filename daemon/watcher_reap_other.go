//go:build !linux && !darwin

package daemon

import "errors"

// waitExitedUnreaped has no implementation on this platform, so the group
// cleanup kill that depends on it is skipped (see reapWatchGroupLeader).
func waitExitedUnreaped(int) error {
	return errors.New("waiting for an exit without reaping is not implemented on this platform")
}
