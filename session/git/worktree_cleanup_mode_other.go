//go:build !darwin

package git

import "os"

// prepareDirectoryEntryRemoval is a no-op off darwin: renaming or unlinking a
// directory needs rights on the parent alone, so a node's own mode can never
// strand its claim.
func prepareDirectoryEntryRemoval(_ *os.File, _ string) error {
	return nil
}
